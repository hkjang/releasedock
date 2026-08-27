package manifest

import (
	"archive/tar"
	"os"
	"path/filepath"
	"testing"
)

func writeImageTar(t *testing.T, name, metadata string) {
	t.Helper()
	f, err := os.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(f)
	if err := tw.WriteHeader(&tar.Header{Name: "manifest.json", Typeflag: tar.TypeReg, Mode: 0o600, Size: int64(len(metadata))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(metadata)); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestLoadAndResolveImages(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "images"), 0o750); err != nil {
		t.Fatal(err)
	}
	manifestYAML := "application: crm\nversion: 2.4.1\nimages:\n  - file: images/api.tar\n    repository: crm/api\n    tag: 2.4.1\n"
	if err := os.WriteFile(filepath.Join(root, "manifest.yaml"), []byte(manifestYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	writeImageTar(t, filepath.Join(root, "images", "api.tar"), `[{"RepoTags":["customer-api:2.4.1"]}]`)
	release, err := Load(root, "manifest.yaml")
	if err != nil {
		t.Fatal(err)
	}
	images, err := ResolveImages(root, release, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(images) != 1 || images[0].SourceRef != "customer-api:2.4.1" || images[0].Repository != "crm/api" {
		t.Fatalf("unexpected images: %#v", images)
	}
}

func TestLoadRejectsUnknownYAMLField(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "manifest.yaml"), []byte("application: x\nversion: v1\ncommand: rm -rf /\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(root, "manifest.yaml"); err == nil {
		t.Fatal("expected strict YAML error")
	}
}

func TestResolveRejectsSymlinkOutsideWorkspace(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "evil.tar")
	writeImageTar(t, outside, `[{"RepoTags":["evil:v1"]}]`)
	if err := os.Symlink(outside, filepath.Join(root, "evil.tar")); err != nil {
		t.Fatal(err)
	}
	_, err := ResolveImages(root, Release{Application: "x", Version: "v1", Images: []ImageSpec{{File: "evil.tar"}}}, 2)
	if err == nil {
		t.Fatal("expected escaping symlink error")
	}
}

func TestResolveRejectsSourceMissingFromArchive(t *testing.T) {
	root := t.TempDir()
	imagePath := filepath.Join(root, "api.tar")
	writeImageTar(t, imagePath, `[{"RepoTags":["trusted-api:2.4.1"]}]`)
	_, err := ResolveImages(root, Release{
		Application: "crm",
		Version:     "2.4.1",
		Images: []ImageSpec{{
			File: "api.tar", Source: "unrelated-existing-image:latest",
		}},
	}, 2)
	if err == nil {
		t.Fatal("expected source outside the uploaded image archive to be rejected")
	}
}
