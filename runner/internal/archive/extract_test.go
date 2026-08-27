package archive

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/hkjang/releasedock/runner/internal/model"
)

type tarEntry struct {
	name     string
	content  string
	typeflag byte
	linkname string
}

func writeTar(t *testing.T, entries []tarEntry, compressed bool) string {
	t.Helper()
	name := filepath.Join(t.TempDir(), "release.tar")
	if compressed {
		name += ".gz"
	}
	f, err := os.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	var tw *tar.Writer
	var gz *gzip.Writer
	if compressed {
		gz = gzip.NewWriter(f)
		tw = tar.NewWriter(gz)
	} else {
		tw = tar.NewWriter(f)
	}
	for _, entry := range entries {
		typeflag := entry.typeflag
		if typeflag == 0 {
			typeflag = tar.TypeReg
		}
		size := int64(len(entry.content))
		if typeflag != tar.TypeReg {
			size = 0
		}
		if err := tw.WriteHeader(&tar.Header{Name: entry.name, Mode: 0o777, Size: size, Typeflag: typeflag, Linkname: entry.linkname}); err != nil {
			t.Fatal(err)
		}
		if size > 0 {
			if _, err := tw.Write([]byte(entry.content)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if gz != nil {
		if err := gz.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return name
}

func policy() model.ExtractionPolicy {
	return model.ExtractionPolicy{MaxArchiveBytes: 1 << 20, MaxExtractedBytes: 1 << 20, MaxFiles: 20}
}

func TestExtractGzip(t *testing.T) {
	archive := writeTar(t, []tarEntry{{name: "manifest.yaml", content: "version: 1.2.3"}, {name: "images/app.tar", content: "image"}}, true)
	destination := t.TempDir()
	result, err := Extract(context.Background(), archive, destination, policy())
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Files) != 2 || result.ExtractedBytes != int64(len("version: 1.2.3image")) {
		t.Fatalf("unexpected result: %#v", result)
	}
	data, err := os.ReadFile(filepath.Join(destination, "manifest.yaml"))
	if err != nil || string(data) != "version: 1.2.3" {
		t.Fatalf("unexpected extracted file: %q, %v", data, err)
	}
}

func TestExtractRejectsTraversal(t *testing.T) {
	archive := writeTar(t, []tarEntry{{name: "../../escape", content: "bad"}}, false)
	if _, err := Extract(context.Background(), archive, t.TempDir(), policy()); err == nil {
		t.Fatal("expected traversal error")
	}
}

func TestExtractRejectsSymlinkPivotEvenWhenLinksAllowed(t *testing.T) {
	archive := writeTar(t, []tarEntry{
		{name: "pivot", typeflag: tar.TypeSymlink, linkname: "inside"},
		{name: "pivot/file", content: "bad"},
	}, false)
	p := policy()
	p.AllowSymlinks = true
	if _, err := Extract(context.Background(), archive, t.TempDir(), p); err == nil {
		t.Fatal("expected symlink parent error")
	}
}

func TestExtractHonorsLimits(t *testing.T) {
	archive := writeTar(t, []tarEntry{{name: "one", content: "12345"}}, false)
	p := policy()
	p.MaxExtractedBytes = 4
	if _, err := Extract(context.Background(), archive, t.TempDir(), p); err == nil {
		t.Fatal("expected extracted byte limit error")
	}

	p = policy()
	p.MaxFiles = 1
	archive = writeTar(t, []tarEntry{{name: "one", content: "1"}, {name: "two", content: "2"}}, false)
	if _, err := Extract(context.Background(), archive, t.TempDir(), p); err == nil {
		t.Fatal("expected entry count limit error")
	}
}
