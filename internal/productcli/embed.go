package productcli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"
)

func embeddedBootstrapSource(root, productID, version string) ([]byte, error) {
	archive, err := archiveProduct(root)
	if err != nil {
		return nil, err
	}
	digest := fmt.Sprintf("%x", sha256.Sum256(archive))
	payload := base64.StdEncoding.EncodeToString(archive)
	source := fmt.Sprintf(`package main

import (
 "archive/tar"
 "compress/gzip"
 "crypto/sha256"
 "encoding/base64"
 "fmt"
 "io"
 "os"
 "path/filepath"
 "strings"
)

const pulpProductPayload = %q
const pulpProductDigest = %q
const pulpProductID = %q
const pulpProductVersion = %q

func init() {
 data, err := base64.StdEncoding.DecodeString(pulpProductPayload)
 if err != nil { panic("decode embedded Pulp product: " + err.Error()) }
 if fmt.Sprintf("%%x", sha256.Sum256(data)) != pulpProductDigest { panic("embedded Pulp product digest mismatch") }
 cache := os.Getenv("PULP_PRODUCT_CACHE")
 if cache == "" {
  cache, err = os.UserCacheDir()
  if err != nil { cache = os.TempDir() }
  cache = filepath.Join(cache, "pulp", "products")
 }
 root := filepath.Join(cache, pulpProductID, pulpProductDigest)
 marker := filepath.Join(root, ".complete")
 if _, err = os.Stat(marker); err != nil {
  if err = os.MkdirAll(cache, 0700); err != nil { panic("create Pulp product cache root: " + err.Error()) }
  temp, makeErr := os.MkdirTemp(cache, ".extract-")
  if makeErr != nil { panic("create Pulp product cache: " + makeErr.Error()) }
  defer os.RemoveAll(temp)
  if err = extractPulpProduct(data, temp); err != nil { panic("extract embedded Pulp product: " + err.Error()) }
  if err = os.WriteFile(filepath.Join(temp, ".complete"), []byte(pulpProductDigest+"\n"), 0600); err != nil { panic(err) }
  if err = os.MkdirAll(filepath.Dir(root), 0700); err != nil { panic(err) }
  if err = os.Rename(temp, root); err != nil {
   if _, statErr := os.Stat(marker); statErr != nil { panic("activate embedded Pulp product: " + err.Error()) }
  }
 }
 for _, arg := range os.Args[1:] {
  if arg == "-host" || arg == "-app" || arg == "-manifest" || strings.HasPrefix(arg, "-host=") || strings.HasPrefix(arg, "-app=") || strings.HasPrefix(arg, "-manifest=") { return }
 }
 os.Args = append(os.Args, "-host", filepath.Join(root, "pulp.host.toml"))
}

func extractPulpProduct(data []byte, root string) error {
 zipped, err := gzip.NewReader(strings.NewReader(string(data)))
 if err != nil { return err }
 defer zipped.Close()
 reader := tar.NewReader(zipped)
 for {
  header, err := reader.Next()
  if err == io.EOF { return nil }
  if err != nil { return err }
  clean := filepath.Clean(filepath.FromSlash(header.Name))
  if clean == "." || filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) { return fmt.Errorf("unsafe embedded path %%q", header.Name) }
  target := filepath.Join(root, clean)
  switch header.Typeflag {
  case tar.TypeDir:
   if err := os.MkdirAll(target, 0700); err != nil { return err }
  case tar.TypeReg:
   if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil { return err }
   file, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, os.FileMode(header.Mode)&0700)
   if err != nil { return err }
   _, copyErr := io.Copy(file, reader)
   closeErr := file.Close()
   if copyErr != nil { return copyErr }
   if closeErr != nil { return closeErr }
  default:
   return fmt.Errorf("unsupported embedded entry %%q", header.Name)
  }
 }
}
`, payload, digest, productID, version)
	return []byte(source), nil
}

func archiveProduct(root string) ([]byte, error) {
	var paths []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == root {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("product archive rejects symlink %s", path)
		}
		if !entry.IsDir() && !entry.Type().IsRegular() {
			return fmt.Errorf("product archive rejects non-regular file %s", path)
		}
		paths = append(paths, path)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	var result bytes.Buffer
	gz, _ := gzip.NewWriterLevel(&result, gzip.BestCompression)
	gz.Header.ModTime = time.Unix(0, 0)
	tarWriter := tar.NewWriter(gz)
	for _, path := range paths {
		info, err := os.Stat(path)
		if err != nil {
			return nil, err
		}
		rel, _ := filepath.Rel(root, path)
		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return nil, err
		}
		header.Name = filepath.ToSlash(rel)
		header.ModTime, header.AccessTime, header.ChangeTime = time.Unix(0, 0), time.Time{}, time.Time{}
		header.Uid, header.Gid, header.Uname, header.Gname = 0, 0, "", ""
		if err := tarWriter.WriteHeader(header); err != nil {
			return nil, err
		}
		if info.Mode().IsRegular() {
			file, err := os.Open(path)
			if err != nil {
				return nil, err
			}
			_, copyErr := io.Copy(tarWriter, file)
			closeErr := file.Close()
			if copyErr != nil {
				return nil, copyErr
			}
			if closeErr != nil {
				return nil, closeErr
			}
		}
	}
	if err := tarWriter.Close(); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return result.Bytes(), nil
}
