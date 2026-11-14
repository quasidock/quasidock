package builder

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/ulikunitz/xz"

	"github.com/quasidock/quasidock/internal/context"
	"github.com/quasidock/quasidock/internal/format"
	"github.com/quasidock/quasidock/internal/oci"
	"github.com/quasidock/quasidock/internal/recipe"
	"github.com/quasidock/quasidock/internal/store"
	"github.com/quasidock/quasidock/internal/tree"
	"github.com/quasidock/quasidock/internal/zipfile"
)

type Sources struct {
	ctx       *context.Context
	fetcher   *Fetcher
	projects  *Snapshots
	openFiles []*os.File
	hashes    []string
	images    []string
}

func NewSources(ctx *context.Context, baseDir, appID string, projects *Snapshots, mirrors ...string) *Sources {
	return &Sources{
		ctx:      ctx,
		fetcher:  NewFetcher(ctx, baseDir, appID, mirrors...),
		projects: projects,
	}
}

func (s *Sources) Close() {
	for _, f := range s.openFiles {
		_ = f.Close()
	}
	s.openFiles = nil
}

func (s *Sources) Add(t *tree.Tree, m recipe.Stage, recipePath string) error {
	for i, src := range m.Sources {
		dest := "."
		if src.Dest != nil {
			dest = *src.Dest
		}
		normDest := tree.NormalizePath(dest)

		if s.projects != nil {
			if snap := s.projects.Get(m.Name, i); snap != nil {
				s.hashes = append(s.hashes, snap.Hash)
				if err := t.AddTree(snap.Tree, normDest); err != nil {
					return fmt.Errorf("%s: %s is a file in one source and a directory in another: %w", recipePath, t.Conflict(), err)
				}
				continue
			}
		}

		if src.Image != nil {
			dep, err := ResolveDependency(s.ctx, recipePath, DependencySource, m.Name, *src.Image)
			if err != nil {
				return err
			}
			s.images = append(s.images, dep.ManifestDigest)
			dirs, locks, err := s.ctx.Store.UseLayers([]oci.Descriptor{dep.Layer()})
			if err != nil {
				return err
			}
			defer func() {
				for _, l := range locks {
					l.Release()
				}
			}()
			if err := t.AddDirFiles(dirs[0], normDest); err != nil {
				return fmt.Errorf("%s: %s is a file in one source and a directory in another: %w", recipePath, t.Conflict(), err)
			}
			continue
		}

		start := s.ctx.Now()
		file, err := s.fetcher.Fetch(src)
		if err != nil {
			return err
		}
		hash := ""
		if src.Sha256 != nil {
			hash = *src.Sha256
		} else {
			h, err := store.Sha256File(file)
			if err != nil {
				return err
			}
			hash = h.Hex
		}
		s.hashes = append(s.hashes, hash)
		s.ctx.Timed(start, "fetch %s", src.FileName())

		start = s.ctx.Now()
		kind := src.SourceKind()
		switch kind {
		case recipe.KindFile:
			stat, err := os.Stat(file)
			if err != nil {
				return fmt.Errorf("module %s: reading %s: %w", m.Name, src.FileName(), err)
			}
			targetName := src.FileName()
			if src.Dest != nil {
				targetName = *src.Dest
			}
			targetNorm := tree.NormalizePath(targetName)
			if err := t.AddFile(targetNorm, tree.Content{
				Kind:     tree.ContentFile,
				FilePath: file,
				FileSize: stat.Size(),
			}); err != nil {
				return fmt.Errorf("%s: %s is a file in one source and a directory in another: %w", recipePath, t.Conflict(), err)
			}

		case recipe.KindZip:
			f, err := os.Open(file)
			if err != nil {
				return fmt.Errorf("module %s: reading %s: %w", m.Name, src.FileName(), err)
			}
			s.openFiles = append(s.openFiles, f)
			fi, err := f.Stat()
			if err != nil {
				return err
			}
			entries, err := zipfile.List(f, fi.Size())
			if err != nil {
				return fmt.Errorf("module %s: reading %s: %w", m.Name, src.FileName(), err)
			}
			if err := t.AddZipEntries(entries, normDest, int(src.Strip)); err != nil {
				return fmt.Errorf("%s: %s is a file in one source and a directory in another: %w", recipePath, t.Conflict(), err)
			}

		case recipe.KindTar, recipe.KindTarGz, recipe.KindTarXz:
			plainTarPath, err := s.plainTar(file, kind, hash)
			if err != nil {
				return fmt.Errorf("module %s: reading %s: %w", m.Name, src.FileName(), err)
			}
			f, err := os.Open(plainTarPath)
			if err != nil {
				return fmt.Errorf("module %s: reading %s: %w", m.Name, src.FileName(), err)
			}
			s.openFiles = append(s.openFiles, f)
			fi, err := f.Stat()
			if err != nil {
				return err
			}
			entries, err := tree.ListTar(f, fi.Size())
			if err != nil {
				return fmt.Errorf("module %s: reading %s: %w", m.Name, src.FileName(), err)
			}
			if err := t.AddTarEntries(f, entries, normDest, int(src.Strip)); err != nil {
				return fmt.Errorf("%s: %s is a file in one source and a directory in another: %w", recipePath, t.Conflict(), err)
			}
		}
		s.ctx.Timed(start, "index %s", src.FileName())
	}
	return nil
}

func (s *Sources) plainTar(file string, kind recipe.Kind, hash string) (string, error) {
	if kind == recipe.KindTar {
		return file, nil
	}
	dest := s.ctx.Store.Path("cache", "downloads", hash+".tar")
	if store.Exists(dest) {
		return dest, nil
	}

	tmp := s.ctx.Store.TmpPath("untar")
	defer func() { _ = os.Remove(tmp) }()

	var err error
	switch kind {
	case recipe.KindTarGz:
		err = format.Inflate(file, tmp)
	case recipe.KindTarXz:
		err = inflateXZ(file, tmp)
	default:
		return "", fmt.Errorf("unsupported tar compression: %s", kind)
	}
	if err != nil {
		return "", err
	}
	_ = os.MkdirAll(filepath.Dir(dest), 0755)
	if err := store.RenameRetrying(tmp, dest, 5000); err != nil {
		return "", err
	}
	return dest, nil
}

func inflateXZ(file, dest string) error {
	in, err := os.Open(file)
	if err != nil {
		return err
	}
	defer in.Close()
	reader, err := xz.NewReader(in)
	if err != nil {
		return err
	}
	out, err := os.Create(dest)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, reader)
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}
