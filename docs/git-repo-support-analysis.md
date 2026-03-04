# Analysis: Supporting HuggingFace Git Repos in hf-mount

## Context

`hf-mount` currently mounts **HuggingFace Buckets** — a flat key-value store backed by Xet CAS.
This document analyzes what it would take to also support **HuggingFace Git repositories**
(models, datasets, spaces) — versioned repos with mixed git blob + LFS + Xet storage.

The goal: a single `hf-mount` binary that can mount either a bucket or a git repo.

---

## API Surface Comparison

| Concern | Buckets (current) | Git Repos (target) |
|---------|-------------------|---------------------|
| **Tree listing** | `GET /api/buckets/{id}/tree[/{prefix}]` | `GET /api/{type}s/{repo_id}/tree/{revision}[/{path}]` |
| **File identity** | `xet_hash` (always) | Mixed: `oid` (git SHA-1) for blobs, `lfs.oid` (SHA-256) for LFS, `xetHash` for Xet-migrated |
| **mtime** | Native `mtime` field on every entry | No native mtime. Requires `expand=true` → `lastCommit.date` (expensive) |
| **Download** | Xet CAS reconstruction via `xet_hash` | `GET /{repo}/resolve/{rev}/{path}` → inline (small) or 302 redirect (LFS/Xet) |
| **Auth tokens** | Per-bucket CAS token: `GET /api/buckets/{id}/xet-read-token` | Repo-level `Authorization: Bearer hf_...` (same token for all ops) |
| **Write (commit)** | Single call: `POST /api/buckets/{id}/batch` (NDJSON: `addFile`/`deleteFile`) | Three-step: preupload → LFS upload to S3 → `POST /api/{type}s/{repo_id}/commit/{rev}` |
| **Revisions** | None (mutable key-value) | Every URL takes `{revision}` (branch, tag, commit SHA) |
| **Pagination** | `Link: <url>; rel="next"` | Same pattern |

### Tree Entry Formats

**Buckets** (current):
```json
{"path": "data/file.bin", "entry_type": "file", "size": 1048576, "xetHash": "abc123...", "mtime": "2024-01-15T10:30:00Z"}
```

**Git repos** — regular file:
```json
{"type": "file", "oid": "10c66461e4c...", "size": 665, "path": "config.json"}
```

**Git repos** — LFS file:
```json
{"type": "file", "oid": "44b36d6e32d...", "size": 548105171, "path": "model.safetensors",
 "lfs": {"oid": "248dfc3911...", "size": 548105171, "pointerSize": 134},
 "xetHash": "63bed80836ee..."}
```

**Git repos** — directory:
```json
{"type": "directory", "oid": "d03ec5ec17...", "size": 0, "path": "onnx"}
```

### Download Flow

**Buckets**: Always Xet CAS reconstruction via `FileDownloadSession` + `xet_hash`.

**Git repos** — three scenarios:
1. **Small git blob** (< LFS threshold): `GET /resolve/{rev}/{path}` returns content inline (HTTP 200)
2. **LFS file on Xet-migrated repo**: `GET /resolve/{rev}/{path}` returns 302 with `X-Xet-Hash` header → use Xet CAS (same as buckets)
3. **LFS file on legacy repo**: `GET /resolve/{rev}/{path}` returns 302 to S3/CDN presigned URL → HTTP range-request download

### Write/Commit Flow

**Buckets**: Single atomic batch call:
```
POST /api/buckets/{id}/batch
{"addFile": {"path": "...", "xetHash": "...", "mtime": ...}}
{"deleteFile": {"path": "..."}}
```

**Git repos**: Three-step process:

1. **Preupload** — determine upload mode per file:
   ```
   POST /api/{type}s/{repo_id}/preupload/{revision}
   {"files": [{"path": "weights.bin", "size": 1073741824, "sample": "<base64>"}]}
   → {"files": [{"path": "weights.bin", "uploadMode": "lfs"}]}
   ```

2. **LFS upload** — upload large files to S3:
   ```
   POST /{repo}.git/info/lfs/objects/batch
   {"operation": "upload", "objects": [{"oid": "<sha256>", "size": ...}]}
   → presigned URL(s) → PUT file content → POST verify
   ```

3. **Commit** — create the git commit:
   ```
   POST /api/{type}s/{repo_id}/commit/{revision}
   Content-Type: application/x-ndjson

   {"key":"header","value":{"summary":"Update files","parentCommit":"abc..."}}
   {"key":"file","value":{"content":"<base64>","path":"small.json","encoding":"base64"}}
   {"key":"lfsFile","value":{"path":"weights.bin","algo":"sha256","oid":"<sha256>","size":...}}
   {"key":"deletedFile","value":{"path":"old-file.bin"}}
   ```

---

## Impact on Current Codebase

### What changes per file

#### `hub_api.rs` — HIGH impact (rewrite)

Every endpoint is bucket-specific. For git repos:
- `list_tree()` → different URL, different response schema, needs `revision` param
- `get_cas_token()` → not needed (use HF bearer token directly, or extract from `resolve` response headers)
- `get_cas_write_token()` → not needed
- `batch_operations()` → replaced by three-step preupload/LFS-upload/commit

**Approach**: Extract a `StorageBackend` trait, with `BucketBackend` and `GitRepoBackend` implementations.

```rust
#[async_trait]
pub trait StorageBackend: Send + Sync {
    /// List files at path
    async fn list_tree(&self, prefix: &str) -> Result<Vec<RemoteEntry>>;

    /// Download file content (range support)
    async fn download(&self, entry: &RemoteEntry, range: Option<Range<u64>>) -> Result<DownloadStream>;

    /// Upload files and commit
    async fn commit(&self, ops: Vec<CommitOp>) -> Result<()>;
}

pub struct RemoteEntry {
    pub path: String,
    pub kind: EntryKind,            // File or Directory
    pub size: u64,
    pub mtime: Option<SystemTime>,
    pub content_id: ContentId,      // abstracts over xet_hash / lfs oid / git oid
}

pub enum ContentId {
    Xet(String),                    // xet_hash — use CAS reconstruction
    Lfs { oid: String, size: u64 }, // LFS SHA-256 — download via /resolve redirect
    GitBlob(String),                // git OID — download via /resolve inline
}

pub enum CommitOp {
    AddFile { path: String, local_path: PathBuf },
    DeleteFile { path: String },
}
```

#### `inode.rs` — MEDIUM impact

Replace `xet_hash: Option<String>` with `content_id: Option<ContentId>`.
All methods that touch `xet_hash` need updating (insert, update_remote_file, file_snapshot).

#### `cache.rs` — MEDIUM impact

`download_to_file()` currently takes `xet_hash` + `size`. Needs to dispatch on `ContentId`:
- `ContentId::Xet` → existing Xet CAS download
- `ContentId::Lfs` → HTTP GET to `/resolve/` URL, follow 302 redirect, download from CDN
- `ContentId::GitBlob` → HTTP GET to `/resolve/` URL, read inline response

`upload_files()` currently uses Xet `FileUploadSession`. For git repos:
- Small files → include base64 content inline in commit NDJSON
- Large files → compute SHA-256, LFS batch upload to S3, reference in commit

#### `auth.rs` — LOW impact

For git repos, a single HF token works for all operations (no per-resource CAS tokens).
The `HubTokenRefresher` / `HubWriteTokenRefresher` are bucket-specific.
Git repos just need the bearer token (no refresh dance for CAS).

However, if the git repo is Xet-migrated and we use CAS for downloads, we'd still need
a CAS token — extractable from the `Link` header of a `/resolve` response.

#### `vfs.rs` — MEDIUM impact

`PrefetchState` stores `xet_hash: String` → replace with `content_id: ContentId`.
Download stream creation dispatches on content ID type.

`flush_batch()` currently:
1. Uploads via `FileUploadSession` (xet-core) → gets `XetFileInfo` with `xet_hash`
2. Commits via `batch_operations()` with `xet_hash`

For git repos:
1. Computes SHA-256 for large files, reads content for small files
2. Calls preupload to determine LFS vs regular
3. Uploads LFS files to S3
4. Creates commit NDJSON with file/lfsFile/deletedFile entries

`poll_remote_changes()` compares `xet_hash` to detect changes → compare `content_id` instead.

#### `main.rs` — MEDIUM impact

CLI needs new args:
- `--repo-id` (alternative to `--bucket-id`)
- `--repo-type` (model/dataset/space, default: model)
- `--revision` (default: main)

Token flow differs: for git repos, just use `--hf-token` directly (no CAS token fetch).
For Xet-migrated repos, may still need CAS client — detect at runtime.

#### `caching_client.rs` — LOW impact

Generic wrapper, works with any CAS client. No changes needed.

---

## Key Challenges

### 1. Mixed Storage Types

Git repos have three file storage types in the same tree:
- **Git blobs**: small files served inline via HTTP
- **LFS**: large files on S3/CDN via presigned URLs
- **Xet CAS**: Xet-migrated LFS files via CAS reconstruction

Each needs a different download path. Buckets have only one (Xet CAS).

**Solution**: `ContentId` enum + dispatch in download/prefetch code.

### 2. No Native mtime

Bucket tree entries have `mtime`. Git tree entries don't — you must use `expand=true`
which adds `lastCommit.date`, but this is the **commit date**, not the file modification time.
It's also an extra API roundtrip per directory.

**Options**:
- A: Use `expand=true` on tree listing (slower, more data)
- B: Use commit date as mtime (semantically wrong but practical)
- C: Use mount time as mtime for all files (simple, loses info)
- D: Lazy-fetch mtime only when `getattr` is called (complex)

**Recommendation**: Option B — use `lastCommit.date` with `expand=true`. The Hub paginates
the same way, so the overhead is manageable.

### 3. Revision Pinning vs Live Branch

Buckets are mutable — there's one "latest" state. Git repos have revisions.
Need to decide:
- Mount a **pinned commit** (immutable, simple, read-only makes sense)
- Mount a **branch head** (mutable, needs polling like buckets, supports writes)

For writes, must mount a branch (not a tag or commit SHA), and commits
need `parentCommit` for conflict detection.

**Recommendation**: Default to branch (`main`), support `--revision=<commit>` for
read-only pinned mounts. When `--revision` is a commit SHA, force read-only.

### 4. Write Path Complexity

Bucket writes: upload file → get xet_hash → single batch call. Simple.

Git repo writes: compute SHA-256 → preupload API → maybe LFS upload to S3
→ create commit NDJSON. Three network roundtrips minimum.

Also: **parentCommit tracking**. Each commit must reference the parent.
If someone else commits between our poll cycles, we get a conflict.
Need retry logic: re-fetch parent commit, rebase our changes, retry.

**Recommendation**: Implement writes as a separate phase. Start read-only for git repos,
add writes later. The bucket write path can coexist unchanged.

### 5. Xet-Migrated Git Repos

Many HF repos are being migrated to Xet storage. These repos have `xetHash` in their
tree entries and support Xet CAS downloads (same protocol as buckets).

This is actually good news: for Xet-migrated repos, the download path is nearly identical
to buckets. The main difference is the tree listing API and commit flow.

**Detection**: If `TreeEntry.xetHash` is present, use CAS. Otherwise, use HTTP download.

---

## Proposed Architecture

```
┌──────────────────────────────────────────────────────┐
│                    hf-mount CLI                       │
│  --bucket-id=xxx  OR  --repo-id=user/model           │
│                       --repo-type=model               │
│                       --revision=main                 │
└────────────────────────┬─────────────────────────────┘
                         │
┌────────────────────────┴─────────────────────────────┐
│              StorageBackend trait                      │
│                                                       │
│  list_tree()    download()    commit()                │
└────────────┬─────────────────────┬───────────────────┘
             │                     │
  ┌──────────┴──────────┐  ┌──────┴──────────────┐
  │  BucketBackend      │  │  GitRepoBackend      │
  │                     │  │                       │
  │  /api/buckets/...   │  │  /api/{type}s/...     │
  │  CAS tokens         │  │  Bearer token         │
  │  Xet download       │  │  Mixed download       │
  │  Batch commit       │  │  Preupload+LFS+Commit │
  └─────────────────────┘  └───────────────────────┘
             │                     │
             └──────────┬──────────┘
                        │
┌───────────────────────┴──────────────────────────────┐
│                    VFS Core                            │
│                                                       │
│  InodeTable (content_id instead of xet_hash)          │
│  FileCache  (dispatch on ContentId)                   │
│  Prefetch   (works with any download stream)          │
│  FlushLoop  (delegates to backend.commit())           │
│  Poller     (delegates to backend.list_tree())        │
└──────────────────────────────────────────────────────┘
             │
      ┌──────┴──────┐
      │             │
   FUSE          NFS (future)
```

---

## Migration Steps

### Step 1: Introduce `ContentId` and `StorageBackend` trait

Replace `xet_hash: Option<String>` throughout with `content_id: Option<ContentId>`.
Create the `StorageBackend` trait. Make current bucket code implement `BucketBackend`.

**Files touched**: `hub_api.rs`, `inode.rs`, `cache.rs`, `vfs.rs`, `main.rs`
**Risk**: Medium (touches every file, but behavior unchanged)
**Verify**: All existing tests pass

### Step 2: Implement `GitRepoBackend` — read-only

Implement `list_tree()` and `download()` for git repos:
- Tree listing with `expand=true` for mtime
- Download dispatch: inline for git blobs, redirect-follow for LFS, CAS for Xet
- Auto-detect Xet support from `xetHash` field presence

**Files touched**: new `git_backend.rs`, `main.rs` (CLI args), `cache.rs` (download dispatch)
**Risk**: Low (additive, doesn't change bucket path)
**Verify**: Mount a public model repo read-only, `ls`, `cat` files

### Step 3: Implement `GitRepoBackend` — writes

Implement `commit()` for git repos:
- Preupload API call
- LFS batch upload for large files
- Commit NDJSON generation
- parentCommit tracking and conflict retry

**Files touched**: `git_backend.rs`, `vfs.rs` (flush_batch delegation)
**Risk**: High (complex multi-step write, conflict handling)
**Verify**: Mount a test repo, create/edit/delete files, verify commits on Hub

---

## Effort Estimate

| Step | Scope | Complexity |
|------|-------|-----------|
| Step 1: ContentId + StorageBackend trait | ~400 lines changed | Medium (refactor) |
| Step 2: GitRepoBackend read-only | ~600 lines new | Medium (new code, well-defined API) |
| Step 3: GitRepoBackend writes | ~800 lines new | High (preupload + LFS + commit + conflicts) |

Total: ~1800 lines of changes/additions.

For comparison, the current `vfs.rs` is ~1600 lines, `hub_api.rs` is ~170 lines.

---

## Open Questions

1. **Xet-migrated repos**: Should we always prefer Xet CAS when `xetHash` is available,
   or should we support falling back to HTTP download? (Xet is faster for large files)

2. **Revision UX**: Should `--revision` be required, or default to `main`?
   What about detached HEAD / tag mounts?

3. **Write conflicts**: What happens when a remote commit lands between our flush cycles?
   Options: fail loudly, auto-retry with rebase, queue and retry.

4. **Small file threshold**: Git repos inline small files in the commit NDJSON (base64).
   What's the cutoff? The Hub's preupload API decides, but we need to handle both paths.

5. **Private repos with gated access**: Do we need special handling for gated models
   (user must accept terms before token grants access)?
