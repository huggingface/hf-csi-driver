package driver

import (
	"testing"
)

func TestBuildArgs_Bucket(t *testing.T) {
	args, err := buildArgs("bucket", "user/my-bucket", "/mnt/target", MountOptions{
		HFToken:     "tok_abc",
		HubEndpoint: "https://huggingface.co",
		CacheDir:    "/cache/vol1",
		ReadOnly:    true,
		ExtraArgs:   []string{"--uid", "1000", "--gid", "1000"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expected := []string{
		"--hf-token", "tok_abc",
		"--hub-endpoint", "https://huggingface.co",
		"--cache-dir", "/cache/vol1",
		"--read-only",
		"--uid", "1000", "--gid", "1000",
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
		"--hf-token", "",
		"repo", "user/my-model", "/mnt/target",
		"--revision", "v1.0",
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

func TestBuildArgs_ExtraArgsPassthrough(t *testing.T) {
	args, err := buildArgs("bucket", "user/b", "/mnt", MountOptions{
		HFToken:     "tok",
		HubEndpoint: "https://hf.co",
		CacheDir:    "/cache",
		ReadOnly:    true,
		ExtraArgs:   []string{"--advanced-writes", "--uid=1000", "--gid=2000"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	flagSet := make(map[string]bool)
	for _, a := range args {
		flagSet[a] = true
	}

	for _, flag := range []string{"--hub-endpoint", "--cache-dir", "--read-only", "--advanced-writes", "--uid=1000", "--gid=2000"} {
		if !flagSet[flag] {
			t.Errorf("missing flag: %s in %v", flag, args)
		}
	}
}
