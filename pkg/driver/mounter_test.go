package driver

import (
	"testing"
)

func TestBuildArgs_Bucket(t *testing.T) {
	args, err := buildArgs("bucket", "user/my-bucket", "/mnt/target", MountOptions{
		HubEndpoint: "https://huggingface.co",
		CacheDir:    "/cache/vol1",
		ReadOnly:    true,
		UID:         "1000",
		GID:         "1000",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expected := []string{
		"--hub-endpoint", "https://huggingface.co",
		"--cache-dir", "/cache/vol1",
		"--read-only",
		"--uid", "1000",
		"--gid", "1000",
		"bucket", "user/my-bucket", "/mnt/target",
	}

	if len(args) != len(expected) {
		t.Fatalf("expected %d args, got %d: %v", len(expected), len(args), args)
	}
	for i, a := range args {
		if a != expected[i] {
			t.Errorf("arg[%d]: expected %q, got %q", i, expected[i], a)
		}
	}
}

func TestBuildArgs_Repo(t *testing.T) {
	args, err := buildArgs("repo", "user/my-model", "/mnt/target", MountOptions{
		Revision: "v1.0",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expected := []string{
		"repo", "user/my-model", "/mnt/target",
		"--revision", "v1.0",
	}
	// Note: repo with no global options, --revision is a subcommand flag

	if len(args) != len(expected) {
		t.Fatalf("expected %d args, got %d: %v", len(expected), len(args), args)
	}
	for i, a := range args {
		if a != expected[i] {
			t.Errorf("arg[%d]: expected %q, got %q", i, expected[i], a)
		}
	}
}

func TestBuildArgs_BucketIgnoresRevision(t *testing.T) {
	args, err := buildArgs("bucket", "user/b", "/mnt", MountOptions{
		Revision: "v1.0",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for _, a := range args {
		if a == "--revision" {
			t.Error("bucket should not include --revision")
		}
	}
}

func TestBuildArgs_InvalidSourceType(t *testing.T) {
	_, err := buildArgs("invalid", "x", "/mnt", MountOptions{})
	if err == nil {
		t.Fatal("expected error for invalid sourceType")
	}
}

func TestBuildArgs_AllOptions(t *testing.T) {
	args, err := buildArgs("bucket", "user/b", "/mnt", MountOptions{
		HubEndpoint:      "https://hf.co",
		CacheDir:         "/cache",
		CacheSize:        "5000000000",
		PollIntervalSecs: "60",
		MetadataTtlMs:    "5000",
		ReadOnly:         true,
		AdvancedWrites:   true,
		UID:              "1000",
		GID:              "2000",
		ExtraArgs:        []string{"--max-threads", "8"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Check key flags are present.
	flagSet := make(map[string]bool)
	for _, a := range args {
		flagSet[a] = true
	}

	for _, flag := range []string{"--hub-endpoint", "--cache-dir", "--cache-size", "--poll-interval-secs", "--metadata-ttl-ms", "--read-only", "--advanced-writes", "--uid", "--gid", "--max-threads"} {
		if !flagSet[flag] {
			t.Errorf("missing flag: %s", flag)
		}
	}
}
