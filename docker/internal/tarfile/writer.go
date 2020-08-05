package tarfile

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/containers/image/v5/docker/reference"
	"github.com/containers/image/v5/manifest"
	"github.com/containers/image/v5/types"
	"github.com/opencontainers/go-digest"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

// Writer allows creating a (docker save)-formatted tar archive containing one or more images.
type Writer struct {
	writer io.Writer
	tar    *tar.Writer
	// Other state.
	blobs map[digest.Digest]types.BlobInfo // list of already-sent blobs
}

// NewWriter returns a Writer for the specified io.Writer.
// The caller must eventually call .Close() on the returned object to create a valid archive.
func NewWriter(dest io.Writer) *Writer {
	return &Writer{
		writer: dest,
		tar:    tar.NewWriter(dest),
		blobs:  make(map[digest.Digest]types.BlobInfo),
	}
}

// tryReusingBlob checks whether the transport already contains, a blob, and if so, returns its metadata.
// info.Digest must not be empty.
// If the blob has been succesfully reused, returns (true, info, nil); info must contain at least a digest and size.
// If the transport can not reuse the requested blob, tryReusingBlob returns (false, {}, nil); it returns a non-nil error only on an unexpected failure.
func (w *Writer) tryReusingBlob(info types.BlobInfo) (bool, types.BlobInfo, error) {
	if info.Digest == "" {
		return false, types.BlobInfo{}, errors.Errorf("Can not check for a blob with unknown digest")
	}
	if blob, ok := w.blobs[info.Digest]; ok {
		return true, types.BlobInfo{Digest: info.Digest, Size: blob.Size}, nil
	}
	return false, types.BlobInfo{}, nil
}

// recordBlob records metadata of a recorded blob, which must contain at least a digest and size.
func (w *Writer) recordBlob(info types.BlobInfo) {
	w.blobs[info.Digest] = info
}

// writeLegacyLayerMetadata writes legacy VERSION and configuration files for all layers
func (w *Writer) writeLegacyLayerMetadata(layerDescriptors []manifest.Schema2Descriptor, configBytes []byte) (layerPaths []string, lastLayerID string, err error) {
	var chainID digest.Digest
	lastLayerID = ""
	for i, l := range layerDescriptors {
		// This chainID value matches the computation in docker/docker/layer.CreateChainID …
		if chainID == "" {
			chainID = l.Digest
		} else {
			chainID = digest.Canonical.FromString(chainID.String() + " " + l.Digest.String())
		}
		// … but note that this image ID does not match docker/docker/image/v1.CreateID. At least recent
		// versions allocate new IDs on load, as long as the IDs we use are unique / cannot loop.
		//
		// Overall, the goal of computing a digest dependent on the full history is to avoid reusing an image ID
		// (and possibly creating a loop in the "parent" links) if a layer with the same DiffID appears two or more
		// times in layersDescriptors.  The ChainID values are sufficient for this, the v1.CreateID computation
		// which also mixes in the full image configuration seems unnecessary, at least as long as we are storing
		// only a single image per tarball, i.e. all DiffID prefixes are unique (can’t differ only with
		// configuration).
		layerID := chainID.Hex()

		physicalLayerPath := w.physicalLayerPath(l.Digest)
		// The layer itself has been stored into physicalLayerPath in PutManifest.
		// So, use that path for layerPaths used in the non-legacy manifest
		layerPaths = append(layerPaths, physicalLayerPath)
		// ... and create a symlink for the legacy format;
		if err := w.sendSymlink(filepath.Join(layerID, legacyLayerFileName), filepath.Join("..", physicalLayerPath)); err != nil {
			return nil, "", errors.Wrap(err, "Error creating layer symbolic link")
		}

		b := []byte("1.0")
		if err := w.sendBytes(filepath.Join(layerID, legacyVersionFileName), b); err != nil {
			return nil, "", errors.Wrap(err, "Error writing VERSION file")
		}

		// The legacy format requires a config file per layer
		layerConfig := make(map[string]interface{})
		layerConfig["id"] = layerID

		// The root layer doesn't have any parent
		if lastLayerID != "" {
			layerConfig["parent"] = lastLayerID
		}
		// The top layer configuration file is generated by using subpart of the image configuration
		if i == len(layerDescriptors)-1 {
			var config map[string]*json.RawMessage
			err := json.Unmarshal(configBytes, &config)
			if err != nil {
				return nil, "", errors.Wrap(err, "Error unmarshaling config")
			}
			for _, attr := range [7]string{"architecture", "config", "container", "container_config", "created", "docker_version", "os"} {
				layerConfig[attr] = config[attr]
			}
		}
		b, err := json.Marshal(layerConfig)
		if err != nil {
			return nil, "", errors.Wrap(err, "Error marshaling layer config")
		}
		if err := w.sendBytes(filepath.Join(layerID, legacyConfigFileName), b); err != nil {
			return nil, "", errors.Wrap(err, "Error writing config json file")
		}

		lastLayerID = layerID
	}
	return layerPaths, lastLayerID, nil
}

func (w *Writer) createRepositoriesFile(rootLayerID string, repoTags []reference.NamedTagged) error {
	repositories := map[string]map[string]string{}
	for _, repoTag := range repoTags {
		if val, ok := repositories[repoTag.Name()]; ok {
			val[repoTag.Tag()] = rootLayerID
		} else {
			repositories[repoTag.Name()] = map[string]string{repoTag.Tag(): rootLayerID}
		}
	}

	b, err := json.Marshal(repositories)
	if err != nil {
		return errors.Wrap(err, "Error marshaling repositories")
	}
	if err := w.sendBytes(legacyRepositoriesFileName, b); err != nil {
		return errors.Wrap(err, "Error writing config json file")
	}
	return nil
}

func (w *Writer) createManifest(configDigest digest.Digest, layerPaths []string, repoTags []reference.NamedTagged) error {
	item := ManifestItem{
		Config:       w.configPath(configDigest),
		RepoTags:     []string{},
		Layers:       layerPaths,
		Parent:       "",
		LayerSources: nil,
	}

	for _, tag := range repoTags {
		// For github.com/docker/docker consumers, this works just as well as
		//   refString := ref.String()
		// because when reading the RepoTags strings, github.com/docker/docker/reference
		// normalizes both of them to the same value.
		//
		// Doing it this way to include the normalized-out `docker.io[/library]` does make
		// a difference for github.com/projectatomic/docker consumers, with the
		// “Add --add-registry and --block-registry options to docker daemon” patch.
		// These consumers treat reference strings which include a hostname and reference
		// strings without a hostname differently.
		//
		// Using the host name here is more explicit about the intent, and it has the same
		// effect as (docker pull) in projectatomic/docker, which tags the result using
		// a hostname-qualified reference.
		// See https://github.com/containers/image/issues/72 for a more detailed
		// analysis and explanation.
		refString := fmt.Sprintf("%s:%s", tag.Name(), tag.Tag())
		item.RepoTags = append(item.RepoTags, refString)
	}

	items := []ManifestItem{item}
	itemsBytes, err := json.Marshal(&items)
	if err != nil {
		return err
	}

	return w.sendBytes(manifestFileName, itemsBytes)
}

// Close writes all outstanding data about images to the archive, and finishes writing data
// to the underlying io.Writer.
// No more images can be added after this is called.
func (w *Writer) Close() error {
	return w.tar.Close()
}

// configPath returns a path we choose for storing a config with the specified digest.
// NOTE: This is an internal implementation detail, not a format property, and can change
// any time.
func (w *Writer) configPath(configDigest digest.Digest) string {
	return configDigest.Hex() + ".json"
}

// physicalLayerPath returns a path we choose for storing a layer with the specified digest
// (the actual path, i.e. a regular file, not a symlink that may be used in the legacy format).
// NOTE: This is an internal implementation detail, not a format property, and can change
// any time.
func (w *Writer) physicalLayerPath(layerDigest digest.Digest) string {
	// Note that this can't be e.g. filepath.Join(l.Digest.Hex(), legacyLayerFileName); due to the way
	// writeLegacyLayerMetadata constructs layer IDs differently from inputinfo.Digest values (as described
	// inside it), most of the layers would end up in subdirectories alone without any metadata; (docker load)
	// tries to load every subdirectory as an image and fails if the config is missing.  So, keep the layers
	// in the root of the tarball.
	return layerDigest.Hex() + ".tar"
}

type tarFI struct {
	path      string
	size      int64
	isSymlink bool
}

func (t *tarFI) Name() string {
	return t.path
}
func (t *tarFI) Size() int64 {
	return t.size
}
func (t *tarFI) Mode() os.FileMode {
	if t.isSymlink {
		return os.ModeSymlink
	}
	return 0444
}
func (t *tarFI) ModTime() time.Time {
	return time.Unix(0, 0)
}
func (t *tarFI) IsDir() bool {
	return false
}
func (t *tarFI) Sys() interface{} {
	return nil
}

// sendSymlink sends a symlink into the tar stream.
func (w *Writer) sendSymlink(path string, target string) error {
	hdr, err := tar.FileInfoHeader(&tarFI{path: path, size: 0, isSymlink: true}, target)
	if err != nil {
		return nil
	}
	logrus.Debugf("Sending as tar link %s -> %s", path, target)
	return w.tar.WriteHeader(hdr)
}

// sendBytes sends a path into the tar stream.
func (w *Writer) sendBytes(path string, b []byte) error {
	return w.sendFile(path, int64(len(b)), bytes.NewReader(b))
}

// sendFile sends a file into the tar stream.
func (w *Writer) sendFile(path string, expectedSize int64, stream io.Reader) error {
	hdr, err := tar.FileInfoHeader(&tarFI{path: path, size: expectedSize}, "")
	if err != nil {
		return nil
	}
	logrus.Debugf("Sending as tar file %s", path)
	if err := w.tar.WriteHeader(hdr); err != nil {
		return err
	}
	// TODO: This can take quite some time, and should ideally be cancellable using a context.Context.
	size, err := io.Copy(w.tar, stream)
	if err != nil {
		return err
	}
	if size != expectedSize {
		return errors.Errorf("Size mismatch when copying %s, expected %d, got %d", path, expectedSize, size)
	}
	return nil
}
