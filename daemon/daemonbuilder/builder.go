package daemonbuilder

import (
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"

	"github.com/Sirupsen/logrus"
	"github.com/docker/distribution/reference"
	"github.com/docker/docker/api"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/builder"
	"github.com/docker/docker/cliconfig"
	"github.com/docker/docker/container"
	"github.com/docker/docker/daemon"
	"github.com/docker/docker/image"
	"github.com/docker/docker/pkg/archive"
	"github.com/docker/docker/pkg/httputils"
	"github.com/docker/docker/pkg/idtools"
	"github.com/docker/docker/pkg/ioutils"
	"github.com/docker/docker/pkg/urlutil"
	"github.com/docker/docker/registry"
	"github.com/docker/docker/runconfig"
)

// Docker implements builder.Docker for the docker Daemon object.
type Docker struct {
	Daemon      *daemon.Daemon
	OutOld      io.Writer
	AuthConfigs map[string]cliconfig.AuthConfig
	Archiver    *archive.Archiver
}

// ensure Docker implements builder.Docker
var _ builder.Docker = Docker{}

// LookupImage looks up a Docker image referenced by `name`.
func (d Docker) LookupImage(name string) (*image.Image, error) {
	return d.Daemon.GetImage(name)
}

// Pull tells Docker to pull image referenced by `name`.
func (d Docker) Pull(name string) (*image.Image, error) {
	ref, err := reference.ParseNamed(name)
	if err != nil {
		return nil, err
	}
	switch ref.(type) {
	case reference.Tagged:
	case reference.Digested:
	default:
		ref, err = reference.WithTag(ref, "latest")
		if err != nil {
			return nil, err
		}
	}

	pullRegistryAuth := &cliconfig.AuthConfig{}
	if len(d.AuthConfigs) > 0 {
		// The request came with a full auth config file, we prefer to use that
		repoInfo, err := d.Daemon.RegistryService.ResolveRepository(ref)
		if err != nil {
			return nil, err
		}

		resolvedConfig := registry.ResolveAuthConfig(
			&cliconfig.ConfigFile{AuthConfigs: d.AuthConfigs},
			repoInfo.Index,
		)
		pullRegistryAuth = &resolvedConfig
	}

	if err := d.Daemon.PullImage(ref, nil, pullRegistryAuth, ioutils.NopWriteCloser(d.OutOld)); err != nil {
		return nil, err
	}

	return d.Daemon.GetImage(name)
}

// Container looks up a Docker container referenced by `id`.
func (d Docker) Container(id string) (*container.Container, error) {
	return d.Daemon.Get(id)
}

// Create creates a new Docker container and returns potential warnings
func (d Docker) Create(cfg *runconfig.Config, hostCfg *runconfig.HostConfig) (*container.Container, []string, error) {
	ccr, err := d.Daemon.ContainerCreate(&daemon.ContainerCreateConfig{
		Name:            "",
		Config:          cfg,
		HostConfig:      hostCfg,
		AdjustCPUShares: true,
	})
	if err != nil {
		return nil, nil, err
	}
	container, err := d.Daemon.Get(ccr.ID)
	if err != nil {
		return nil, ccr.Warnings, err
	}

	return container, ccr.Warnings, d.Mount(container)
}

// Remove removes a container specified by `id`.
func (d Docker) Remove(id string, cfg *types.ContainerRmConfig) error {
	return d.Daemon.ContainerRm(id, cfg)
}

// Commit creates a new Docker image from an existing Docker container.
func (d Docker) Commit(name string, cfg *types.ContainerCommitConfig) (string, error) {
	return d.Daemon.Commit(name, cfg)
}

// Retain retains an image avoiding it to be removed or overwritten until a corresponding Release() call.
func (d Docker) Retain(sessionID, imgID string) {
	// FIXME: This will be solved with tags in client-side builder
	//d.Daemon.Graph().Retain(sessionID, imgID)
}

// Release releases a list of images that were retained for the time of a build.
func (d Docker) Release(sessionID string, activeImages []string) {
	// FIXME: This will be solved with tags in client-side builder
	//d.Daemon.Graph().Release(sessionID, activeImages...)
}

// Copy copies/extracts a source FileInfo to a destination path inside a container
// specified by a container object.
// TODO: make sure callers don't unnecessarily convert destPath with filepath.FromSlash (Copy does it already).
// Copy should take in abstract paths (with slashes) and the implementation should convert it to OS-specific paths.
func (d Docker) Copy(c *container.Container, destPath string, src builder.FileInfo, decompress bool) error {
	srcPath := src.Path()
	destExists := true
	rootUID, rootGID := d.Daemon.GetRemappedUIDGID()

	// Work in daemon-local OS specific file paths
	destPath = filepath.FromSlash(destPath)

	dest, err := c.GetResourcePath(destPath)
	if err != nil {
		return err
	}

	// Preserve the trailing slash
	// TODO: why are we appending another path separator if there was already one?
	if strings.HasSuffix(destPath, string(os.PathSeparator)) || destPath == "." {
		dest += string(os.PathSeparator)
	}

	destPath = dest

	destStat, err := os.Stat(destPath)
	if err != nil {
		if !os.IsNotExist(err) {
			logrus.Errorf("Error performing os.Stat on %s. %s", destPath, err)
			return err
		}
		destExists = false
	}

	if src.IsDir() {
		// copy as directory
		if err := d.Archiver.CopyWithTar(srcPath, destPath); err != nil {
			return err
		}
		return fixPermissions(srcPath, destPath, rootUID, rootGID, destExists)
	}
	if decompress && archive.IsArchivePath(srcPath) {
		// Only try to untar if it is a file and that we've been told to decompress (when ADD-ing a remote file)

		// First try to unpack the source as an archive
		// to support the untar feature we need to clean up the path a little bit
		// because tar is very forgiving.  First we need to strip off the archive's
		// filename from the path but this is only added if it does not end in slash
		tarDest := destPath
		if strings.HasSuffix(tarDest, string(os.PathSeparator)) {
			tarDest = filepath.Dir(destPath)
		}

		// try to successfully untar the orig
		err := d.Archiver.UntarPath(srcPath, tarDest)
		if err != nil {
			logrus.Errorf("Couldn't untar to %s: %v", tarDest, err)
		}
		return err
	}

	// only needed for fixPermissions, but might as well put it before CopyFileWithTar
	if destExists && destStat.IsDir() {
		destPath = filepath.Join(destPath, src.Name())
	}

	if err := idtools.MkdirAllNewAs(filepath.Dir(destPath), 0755, rootUID, rootGID); err != nil {
		return err
	}
	if err := d.Archiver.CopyFileWithTar(srcPath, destPath); err != nil {
		return err
	}

	return fixPermissions(srcPath, destPath, rootUID, rootGID, destExists)
}

// GetCachedImage returns a reference to a cached image whose parent equals `parent`
// and runconfig equals `cfg`. A cache miss is expected to return an empty ID and a nil error.
func (d Docker) GetCachedImage(imgID string, cfg *runconfig.Config) (string, error) {
	cache, err := d.Daemon.ImageGetCached(image.ID(imgID), cfg)
	if cache == nil || err != nil {
		return "", err
	}
	return cache.ID().String(), nil
}

// Kill stops the container execution abruptly.
func (d Docker) Kill(container *container.Container) error {
	return d.Daemon.Kill(container)
}

// Mount mounts the root filesystem for the container.
func (d Docker) Mount(c *container.Container) error {
	return d.Daemon.Mount(c)
}

// Unmount unmounts the root filesystem for the container.
func (d Docker) Unmount(c *container.Container) error {
	d.Daemon.Unmount(c)
	return nil
}

// Start starts a container
func (d Docker) Start(c *container.Container) error {
	return d.Daemon.Start(c)
}

// Following is specific to builder contexts

// DetectContextFromRemoteURL returns a context and in certain cases the name of the dockerfile to be used
// irrespective of user input.
// progressReader is only used if remoteURL is actually a URL (not empty, and not a Git endpoint).
func DetectContextFromRemoteURL(r io.ReadCloser, remoteURL string, createProgressReader func(in io.ReadCloser) io.ReadCloser) (context builder.ModifiableContext, dockerfileName string, err error) {
	switch {
	case remoteURL == "":
		context, err = builder.MakeTarSumContext(r)
	case urlutil.IsGitURL(remoteURL):
		context, err = builder.MakeGitContext(remoteURL)
	case urlutil.IsURL(remoteURL):
		context, err = builder.MakeRemoteContext(remoteURL, map[string]func(io.ReadCloser) (io.ReadCloser, error){
			httputils.MimeTypes.TextPlain: func(rc io.ReadCloser) (io.ReadCloser, error) {
				dockerfile, err := ioutil.ReadAll(rc)
				if err != nil {
					return nil, err
				}

				// dockerfileName is set to signal that the remote was interpreted as a single Dockerfile, in which case the caller
				// should use dockerfileName as the new name for the Dockerfile, irrespective of any other user input.
				dockerfileName = api.DefaultDockerfileName

				// TODO: return a context without tarsum
				return archive.Generate(dockerfileName, string(dockerfile))
			},
			// fallback handler (tar context)
			"": func(rc io.ReadCloser) (io.ReadCloser, error) {
				return createProgressReader(rc), nil
			},
		})
	default:
		err = fmt.Errorf("remoteURL (%s) could not be recognized as URL", remoteURL)
	}
	return
}
