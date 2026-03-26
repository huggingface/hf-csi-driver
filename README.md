# hf-csi-driver

Kubernetes CSI driver for mounting [Hugging Face Buckets](https://huggingface.co/docs/hub/storage-buckets) and model/dataset repos as FUSE volumes in pods.

Wraps [hf-mount](https://github.com/huggingface/hf-mount) (Rust FUSE filesystem) behind the CSI interface so kubelet can manage mount lifecycle automatically.

## How it works

```
Pod → kubelet → CSI NodePublishVolume → hf-mount-fuse → FUSE mount
                CSI NodeUnpublishVolume → SIGTERM → fusermount -uz
```

- **Node-only driver**: single DaemonSet, no controller, no provisioner
- **Static provisioning**: users create PV/PVC pairs pointing to a bucket or repo
- **HF token**: passed via Kubernetes Secret through `nodePublishSecretRef`
- **Mount flags passthrough**: PV `mountOptions` are forwarded as `--flag` arguments to hf-mount-fuse

## Prerequisites

- Kubernetes 1.26+
- FUSE support on nodes (`/dev/fuse` available, `fuse3` installed)
- The CSI driver container runs as `privileged` (required for FUSE + mount propagation)

## Installation

### Helm (recommended)

```bash
helm install hf-csi oci://ghcr.io/huggingface/charts/hf-csi-driver \
  --namespace kube-system
```

Or from a local checkout:

```bash
helm install hf-csi deploy/helm/hf-csi-driver/ \
  --namespace kube-system
```

### Plain manifests

```bash
kubectl apply -f deploy/kubernetes/serviceaccount.yaml
kubectl apply -f deploy/kubernetes/csidriver.yaml
kubectl apply -f deploy/kubernetes/rbac.yaml
kubectl apply -f deploy/kubernetes/crd.yaml
kubectl apply -f deploy/kubernetes/daemonset.yaml
```

## Usage

### 1. Create a Secret with your HF token

```bash
kubectl create secret generic hf-token --from-literal=token=hf_xxxxx
```

### 2. Ephemeral volume (simplest)

No PV/PVC needed. The volume is created inline in the Pod spec and destroyed with the pod.

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: my-app
spec:
  containers:
    - name: app
      image: python:3.12
      command: ["python", "-c", "import os; print(os.listdir('/model'))"]
      volumeMounts:
        - name: gpt2
          mountPath: /model
          readOnly: true
  volumes:
    - name: gpt2
      csi:
        driver: hf.csi.huggingface.co
        readOnly: true
        volumeAttributes:
          sourceType: repo
          sourceId: openai-community/gpt2
        nodePublishSecretRef:
          name: hf-token
```

### 3. Mount a bucket (read-write, PV/PVC)

```yaml
apiVersion: v1
kind: PersistentVolume
metadata:
  name: my-bucket-pv
spec:
  capacity:
    storage: 1Ti  # ignored by CSI, required by k8s
  accessModes: [ReadWriteMany]
  persistentVolumeReclaimPolicy: Retain
  storageClassName: ""
  csi:
    driver: hf.csi.huggingface.co
    volumeHandle: my-bucket
    nodePublishSecretRef:
      name: hf-token
      namespace: default
    volumeAttributes:
      sourceType: bucket
      sourceId: username/my-bucket
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: my-bucket-pvc
spec:
  accessModes: [ReadWriteMany]
  storageClassName: ""
  resources:
    requests:
      storage: 1Ti
  volumeName: my-bucket-pv
```

### 4. Mount a model repo (read-only, PV/PVC)

```yaml
apiVersion: v1
kind: PersistentVolume
metadata:
  name: gpt2-pv
spec:
  capacity:
    storage: 1Ti
  accessModes: [ReadOnlyMany]
  persistentVolumeReclaimPolicy: Retain
  storageClassName: ""
  mountOptions:
    - read-only
  csi:
    driver: hf.csi.huggingface.co
    volumeHandle: gpt2
    nodePublishSecretRef:
      name: hf-token
      namespace: default
    volumeAttributes:
      sourceType: repo
      sourceId: openai-community/gpt2
      revision: main
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: gpt2-pvc
spec:
  accessModes: [ReadOnlyMany]
  storageClassName: ""
  resources:
    requests:
      storage: 1Ti
  volumeName: gpt2-pv
```

### 5. Use in a pod

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: my-app
spec:
  containers:
    - name: app
      image: python:3.12
      command: ["python", "-c", "import os; print(os.listdir('/data'))"]
      volumeMounts:
        - name: hf-data
          mountPath: /data
          readOnly: true
  volumes:
    - name: hf-data
      persistentVolumeClaim:
        claimName: gpt2-pvc
```

## Volume attributes

Configured in `volumeAttributes` of the PV's CSI section:

| Attribute | Required | Default | Description |
| --- | --- | --- | --- |
| `sourceType` | yes | | `bucket` or `repo` |
| `sourceId` | yes | | HF identifier (e.g. `username/my-bucket`, `openai-community/gpt2`) |
| `revision` | no | `main` | Git revision (repos only) |
| `hubEndpoint` | no | `https://huggingface.co` | Hub API endpoint |
| `cacheDir` | no | auto | Local cache directory for this volume |
| `cacheSize` | no | `10000000000` | Max cache size in bytes |
| `pollIntervalSecs` | no | `30` | Remote change polling interval |
| `metadataTtlMs` | no | `10000` | Kernel metadata cache TTL in milliseconds |

## Mount options

PV `mountOptions` are forwarded as CLI flags to hf-mount-fuse. For example:

```yaml
mountOptions:
  - read-only
  - uid=1000
  - gid=1000
  - advanced-writes
```

## Building

```bash
# Docker image (multi-stage: Rust + Go)
make docker-build

# Go binary only
make build

# Tests
make test
```

## Architecture

```mermaid
graph TD
    subgraph DS["DaemonSet (per node)"]
        CSI["<b>hf-csi-plugin</b><br/><i>Go / CSI gRPC server</i>"]
        REG["<b>node-driver-registrar</b><br/><i>sidecar</i>"]
        LP["<b>liveness-probe</b><br/><i>sidecar</i>"]

        CSI -->|NodePublishVolume| FUSE["<b>hf-mount-fuse</b><br/><i>Rust / one process per volume</i>"]
        CSI -->|NodeUnpublishVolume| FUSE
    end

    KUBELET["kubelet"] -->|CSI gRPC| CSI
    REG -->|registration| KUBELET
    FUSE --> DEV["/dev/fuse"]
    DEV --> MNT["/var/lib/kubelet/pods/.../mount"]
    MNT --> POD["Pod volume mount"]

    FUSE -->|lazy fetch| HF["HF Storage"]
    FUSE -->|metadata + commits| HUB["Hub API"]
```

## License

Apache-2.0
