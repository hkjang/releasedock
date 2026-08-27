package manifest

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

const maxManifestBytes = 1 << 20
const maxImageMetadataBytes = 8 << 20

var (
	tagPattern        = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)
	repositoryPattern = regexp.MustCompile(`^[a-z0-9]+(?:[._-][a-z0-9]+)*(?:/[a-z0-9]+(?:[._-][a-z0-9]+)*)*$`)
)

type Release struct {
	Application string      `yaml:"application"`
	Version     string      `yaml:"version"`
	Images      []ImageSpec `yaml:"images"`
}

type ImageSpec struct {
	File       string `yaml:"file"`
	Source     string `yaml:"source,omitempty"`
	Repository string `yaml:"repository,omitempty"`
	Tag        string `yaml:"tag,omitempty"`
}

type ResolvedImage struct {
	FilePath   string
	SourceRef  string
	Repository string
	Tag        string
	FoundTags  []string
}

func Load(root, relativePath string) (Release, error) {
	if relativePath == "" {
		relativePath = "manifest.yaml"
	}
	fullPath, err := safeFile(root, relativePath)
	if err != nil {
		return Release{}, fmt.Errorf("manifest path: %w", err)
	}
	f, err := os.Open(fullPath)
	if err != nil {
		return Release{}, fmt.Errorf("open manifest: %w", err)
	}
	defer f.Close()
	limited := io.LimitReader(f, maxManifestBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return Release{}, fmt.Errorf("read manifest: %w", err)
	}
	if len(data) > maxManifestBytes {
		return Release{}, fmt.Errorf("manifest exceeds %d bytes", maxManifestBytes)
	}
	var release Release
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&release); err != nil {
		return Release{}, fmt.Errorf("parse manifest YAML: %w", err)
	}
	if release.Application == "" || release.Version == "" {
		return Release{}, errors.New("manifest application and version are required")
	}
	for i, image := range release.Images {
		if image.File == "" {
			return Release{}, fmt.Errorf("images[%d].file is required", i)
		}
		if image.Repository != "" && !repositoryPattern.MatchString(image.Repository) {
			return Release{}, fmt.Errorf("images[%d].repository is invalid", i)
		}
		if image.Tag != "" && !tagPattern.MatchString(image.Tag) {
			return Release{}, fmt.Errorf("images[%d].tag is invalid", i)
		}
	}
	return release, nil
}

func ResolveImages(root string, release Release, maxImages int) ([]ResolvedImage, error) {
	if maxImages <= 0 {
		return nil, errors.New("max images must be positive")
	}
	specs := append([]ImageSpec(nil), release.Images...)
	if len(specs) == 0 {
		discovered, err := discoverImageTars(root, maxImages)
		if err != nil {
			return nil, err
		}
		for _, name := range discovered {
			specs = append(specs, ImageSpec{File: name})
		}
	}
	if len(specs) > maxImages {
		return nil, fmt.Errorf("image count %d exceeds limit %d", len(specs), maxImages)
	}

	resolved := make([]ResolvedImage, 0, len(specs))
	seenFiles := make(map[string]struct{}, len(specs))
	for i, spec := range specs {
		fullPath, err := safeFile(root, spec.File)
		if err != nil {
			return nil, fmt.Errorf("images[%d].file: %w", i, err)
		}
		if strings.ToLower(filepath.Ext(fullPath)) != ".tar" {
			return nil, fmt.Errorf("images[%d].file must be a .tar archive", i)
		}
		if _, exists := seenFiles[fullPath]; exists {
			return nil, fmt.Errorf("duplicate image file %q", spec.File)
		}
		seenFiles[fullPath] = struct{}{}
		tags, err := InspectImageTar(fullPath)
		if err != nil {
			return nil, fmt.Errorf("inspect image %q: %w", spec.File, err)
		}
		source := spec.Source
		if source == "" && len(tags) > 0 {
			source = tags[0]
		}
		if source == "" {
			return nil, fmt.Errorf("image %q has no source reference; set images[].source", spec.File)
		}
		if !containsString(tags, source) {
			return nil, fmt.Errorf("image %q source %q is not present in the uploaded image archive", spec.File, source)
		}
		repository, tag := spec.Repository, spec.Tag
		if repository == "" || tag == "" {
			parsedRepository, parsedTag, err := splitImageRef(source)
			if err != nil {
				return nil, fmt.Errorf("derive destination for image %q: %w", spec.File, err)
			}
			if repository == "" {
				repository = parsedRepository
			}
			if tag == "" {
				tag = parsedTag
			}
		}
		if !repositoryPattern.MatchString(repository) || !tagPattern.MatchString(tag) {
			return nil, fmt.Errorf("image %q has invalid destination repository or tag", spec.File)
		}
		resolved = append(resolved, ResolvedImage{FilePath: fullPath, SourceRef: source, Repository: repository, Tag: tag, FoundTags: tags})
	}
	return resolved, nil
}

func discoverImageTars(root string, maxImages int) ([]string, error) {
	imagesRoot := filepath.Join(root, "images")
	info, err := os.Lstat(imagesRoot)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("inspect images directory: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("images must be a real directory")
	}
	var files []string
	err = filepath.WalkDir(imagesRoot, func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !entry.Type().IsRegular() || strings.ToLower(filepath.Ext(name)) != ".tar" {
			return nil
		}
		relative, err := filepath.Rel(root, name)
		if err != nil {
			return err
		}
		files = append(files, filepath.ToSlash(relative))
		if len(files) > maxImages {
			return fmt.Errorf("discovered image count exceeds limit %d", maxImages)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("discover image archives: %w", err)
	}
	sort.Strings(files)
	return files, nil
}

func InspectImageTar(name string) ([]string, error) {
	f, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	tr := tar.NewReader(f)
	var tags []string
	for entryCount := 0; ; entryCount++ {
		if entryCount > 100000 {
			return nil, errors.New("image archive has too many entries")
		}
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		cleaned := filepath.ToSlash(filepath.Clean(header.Name))
		switch cleaned {
		case "manifest.json":
			data, err := readSmallTarEntry(tr, header.Size)
			if err != nil {
				return nil, err
			}
			var dockerManifest []struct {
				RepoTags []string `json:"RepoTags"`
			}
			if err := json.Unmarshal(data, &dockerManifest); err != nil {
				return nil, fmt.Errorf("parse Docker manifest.json: %w", err)
			}
			for _, item := range dockerManifest {
				tags = append(tags, item.RepoTags...)
			}
		case "index.json":
			data, err := readSmallTarEntry(tr, header.Size)
			if err != nil {
				return nil, err
			}
			var index struct {
				Manifests []struct {
					Annotations map[string]string `json:"annotations"`
				} `json:"manifests"`
			}
			if err := json.Unmarshal(data, &index); err != nil {
				return nil, fmt.Errorf("parse OCI index.json: %w", err)
			}
			for _, item := range index.Manifests {
				if ref := item.Annotations["org.opencontainers.image.ref.name"]; ref != "" {
					tags = append(tags, ref)
				}
			}
		}
	}
	tags = uniqueStrings(tags)
	if len(tags) == 0 {
		return nil, errors.New("no image tags found in Docker manifest.json or OCI index.json")
	}
	return tags, nil
}

func readSmallTarEntry(r io.Reader, size int64) ([]byte, error) {
	if size < 0 || size > maxImageMetadataBytes {
		return nil, fmt.Errorf("image metadata entry exceeds %d bytes", maxImageMetadataBytes)
	}
	return io.ReadAll(io.LimitReader(r, size))
}

func safeFile(root, relative string) (string, error) {
	if relative == "" || filepath.IsAbs(relative) || strings.Contains(relative, "\\") || strings.ContainsRune(relative, '\x00') {
		return "", errors.New("path must be a non-empty relative slash path")
	}
	cleaned := filepath.Clean(filepath.FromSlash(relative))
	if cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return "", errors.New("path traversal")
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	fullPath := filepath.Join(rootAbs, cleaned)
	realPath, err := filepath.EvalSymlinks(fullPath)
	if err != nil {
		return "", err
	}
	if err := ensureWithin(rootAbs, realPath); err != nil {
		return "", err
	}
	info, err := os.Lstat(fullPath)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("path is not a regular file")
	}
	return fullPath, nil
}

func ensureWithin(root, candidate string) error {
	rel, err := filepath.Rel(root, candidate)
	if err != nil {
		return err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return errors.New("path escapes workspace")
	}
	return nil
}

func splitImageRef(ref string) (string, string, error) {
	if strings.ContainsAny(ref, "@ \t\r\n") {
		return "", "", errors.New("digest and whitespace references are not accepted as a tag source")
	}
	lastSlash := strings.LastIndexByte(ref, '/')
	lastColon := strings.LastIndexByte(ref, ':')
	if lastColon <= lastSlash || lastColon == len(ref)-1 {
		return "", "", errors.New("image source must include a tag")
	}
	repository := ref[:lastColon]
	// Strip an existing registry host; destination paths are always relative to
	// the administrator-selected Harbor project.
	if slash := strings.IndexByte(repository, '/'); slash >= 0 {
		first := repository[:slash]
		if strings.Contains(first, ".") || strings.Contains(first, ":") || first == "localhost" {
			repository = repository[slash+1:]
		}
	}
	tag := ref[lastColon+1:]
	if !repositoryPattern.MatchString(repository) || !tagPattern.MatchString(tag) {
		return "", "", errors.New("invalid image repository or tag")
	}
	return repository, tag, nil
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if _, ok := seen[value]; ok || value == "" {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
