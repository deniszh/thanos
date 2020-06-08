// Copyright (c) The Thanos Authors.
// Licensed under the Apache License 2.0.

// Package swift implements common object storage abstractions against OpenStack swift APIs.
package swift

import (
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-kit/kit/log/level"

	"github.com/thanos-io/thanos/pkg/runutil"

	"github.com/go-kit/kit/log"
	"github.com/ncw/swift"
	"github.com/pkg/errors"
	"gopkg.in/yaml.v2"

	"github.com/thanos-io/thanos/pkg/objstore"
)

// DirDelim is the delimiter used to model a directory structure in an object store bucket.
const DirDelim = '/'

var DefaultConfig = Config{
	ChunkSize:      1024 * 1024 * 1024,
	Retries:        3,
	ConnectTimeout: "10s",
	Timeout:        "5m",
}

type Config struct {
	AuthVersion          int    `yaml:"auth_version"`
	AuthUrl              string `yaml:"auth_url"`
	Username             string `yaml:"username"`
	UserDomainName       string `yaml:"user_domain_name"`
	UserDomainID         string `yaml:"user_domain_id"`
	UserId               string `yaml:"user_id"`
	Password             string `yaml:"password"`
	DomainId             string `yaml:"domain_id"`
	DomainName           string `yaml:"domain_name"`
	ProjectID            string `yaml:"project_id"`
	ProjectName          string `yaml:"project_name"`
	ProjectDomainID      string `yaml:"project_domain_id"`
	ProjectDomainName    string `yaml:"project_domain_name"`
	RegionName           string `yaml:"region_name"`
	ContainerName        string `yaml:"container_name"`
	ChunkSize            int64  `yaml:"large_object_chunk_size"`
	SegmentContainerName string `yaml:"large_object_segments_container_name"`
	Retries              int    `yaml:"retries"`
	ConnectTimeout       string `yaml:"connect_timeout"`
	Timeout              string `yaml:"timeout"`
}

func parseConfig(conf []byte) (*Config, error) {
	sc := DefaultConfig
	err := yaml.UnmarshalStrict(conf, &sc)
	return &sc, err
}

func configFromEnv() (*Config, error) {
	c := swift.Connection{}
	if err := c.ApplyEnvironment(); err != nil {
		return nil, err
	}

	config := Config{
		AuthVersion:          c.AuthVersion,
		AuthUrl:              c.AuthUrl,
		Password:             c.ApiKey,
		Username:             c.UserName,
		UserId:               c.UserId,
		DomainId:             c.DomainId,
		DomainName:           c.Domain,
		ProjectID:            c.TenantId,
		ProjectName:          c.Tenant,
		ProjectDomainID:      c.TenantDomainId,
		ProjectDomainName:    c.TenantDomain,
		RegionName:           c.Region,
		ContainerName:        os.Getenv("OS_CONTAINER_NAME"),
		SegmentContainerName: os.Getenv("SWIFT_SEGMENTS_CONTAINER_NAME"),
		Retries:              c.Retries,
		ConnectTimeout:       c.ConnectTimeout.String(),
		Timeout:              c.Timeout.String(),
	}
	if os.Getenv("SWIFT_CHUNK_SIZE") != "" {
		var err error
		config.ChunkSize, err = strconv.ParseInt(os.Getenv("SWIFT_CHUNK_SIZE"), 10, 64)
		if err != nil {
			return nil, errors.Wrap(err, "swift parsing chunk size")
		}
	}
	return &config, nil
}

func connectionFromConfig(sc *Config) (*swift.Connection, error) {
	connection := swift.Connection{
		Domain:         sc.DomainName,
		DomainId:       sc.DomainId,
		UserName:       sc.Username,
		UserId:         sc.UserId,
		ApiKey:         sc.Password,
		AuthUrl:        sc.AuthUrl,
		Retries:        sc.Retries,
		Region:         sc.RegionName,
		AuthVersion:    sc.AuthVersion,
		Tenant:         sc.ProjectName,
		TenantId:       sc.ProjectID,
		TenantDomain:   sc.ProjectDomainName,
		TenantDomainId: sc.ProjectDomainID,
	}
	if sc.ConnectTimeout != "" {
		connectTimeout, err := time.ParseDuration(sc.ConnectTimeout)
		if err != nil {
			return nil, errors.Wrap(err, "swift parsing connectionTimeout")
		}
		connection.ConnectTimeout = connectTimeout
	}
	if sc.Timeout != "" {
		timeout, err := time.ParseDuration(sc.Timeout)
		if err != nil {
			return nil, errors.Wrap(err, "swift parsing timeout")
		}
		connection.Timeout = timeout
	}
	return &connection, nil
}

type Container struct {
	logger            log.Logger
	name              string
	connection        *swift.Connection
	chunkSize         int64
	segmentsContainer string
}

func NewContainer(logger log.Logger, conf []byte) (*Container, error) {
	sc, err := parseConfig(conf)
	if err != nil {
		return nil, errors.Wrap(err, "swift parse configs")
	}
	return NewContainerFromConfig(logger, sc, false)
}

func ensureContainer(connection *swift.Connection, name string, createIfNotExist bool) (string, error) {
	if _, _, err := connection.Container(name); err != nil {
		if err != swift.ContainerNotFound {
			return "", errors.Wrapf(err, "swift verify container %s", name)
		}
		if !createIfNotExist {
			return "", fmt.Errorf("unable to find the expected container %s", name)
		}
		var newContainer swift.Container
		if err = connection.ContainerCreate(name, swift.Headers{}); err != nil {
			return "", errors.Wrapf(err, "create container %s", name)
		}
		return newContainer.Name, nil
	}
	return "", nil
}

func NewContainerFromConfig(logger log.Logger, sc *Config, createContainer bool) (*Container, error) {
	connection, err := connectionFromConfig(sc)
	if err != nil {
		return nil, errors.Wrap(err, "swift load config")
	}

	if err := connection.Authenticate(); err != nil {
		return nil, errors.Wrap(err, "swift authentication")
	}

	if _, err := ensureContainer(connection, sc.ContainerName, createContainer); err != nil {
		return nil, err
	}
	if sc.SegmentContainerName == "" {
		sc.SegmentContainerName = sc.ContainerName
	} else if _, err := ensureContainer(connection, sc.SegmentContainerName, createContainer); err != nil {
			return nil, err
		}
	}

	container := Container{
		logger:            logger,
		name:              sc.ContainerName,
		connection:        connection,
		chunkSize:         sc.ChunkSize,
		segmentsContainer: sc.SegmentContainerName,
	}
	return &container, nil
}

// Name returns the container name for swift.
func (c *Container) Name() string {
	return c.name
}

// Iter calls f for each entry in the given directory. The argument to f is the full
// object name including the prefix of the inspected directory.
func (c *Container) Iter(ctx context.Context, dir string, f func(string) error) error {
	if dir != "" {
		dir = strings.TrimSuffix(dir, string(DirDelim)) + string(DirDelim)
	}
	return c.connection.ObjectsWalk(c.name, &swift.ObjectsOpts{Prefix: dir, Delimiter: DirDelim}, func(opts *swift.ObjectsOpts) (interface{}, error) {
		objects, err := c.connection.ObjectNames(c.name, opts)
		if err != nil {
			return objects, errors.Wrap(err, "swift list object names")
		}
		for _, object := range objects {
			if err := f(object); err != nil {
				return objects, errors.Wrap(err, "swift iteration over objects")
			}
		}
		return objects, nil
	})
}

func (c *Container) get(name string, headers swift.Headers, checkHash bool) (io.ReadCloser, error) {
	if name == "" {
		return nil, errors.New("Object name cannot be empty")
	}
	file, _, err := c.connection.ObjectOpen(c.name, name, checkHash, headers)
	if err != nil {
		return nil, errors.Wrap(err, "swift open object")
	}
	return file, err
}

// Get returns a reader for the given object name.
func (c *Container) Get(ctx context.Context, name string) (io.ReadCloser, error) {
	return c.get(name, swift.Headers{}, true)
}

func (c *Container) GetRange(ctx context.Context, name string, off, length int64) (io.ReadCloser, error) {
	// Set Range HTTP header, see the docs https://docs.openstack.org/api-ref/object-store/?expanded=show-container-details-and-list-objects-detail,get-object-content-and-metadata-detail#id76.
	bytesRange := fmt.Sprintf("bytes=%d-", off)
	if length != -1 {
		bytesRange = fmt.Sprintf("%s%d", bytesRange, off+length-1)
	}
	return c.get(name, swift.Headers{"Range": bytesRange}, false)
}

// Attributes returns information about the specified object.
func (c *Container) Attributes(ctx context.Context, name string) (objstore.ObjectAttributes, error) {
	if name == "" {
		return objstore.ObjectAttributes{}, errors.New("Object name cannot be empty")
	}
	info, _, err := c.connection.Object(c.name, name)
	if err != nil {
		return objstore.ObjectAttributes{}, errors.Wrap(err, "swift get object attributes")
	}
	return objstore.ObjectAttributes{
		Size:         info.Bytes,
		LastModified: info.LastModified,
	}, nil
}

// Exists checks if the given object exists.
func (c *Container) Exists(ctx context.Context, name string) (bool, error) {
	_, _, err := c.connection.Object(c.name, name)
	if err != nil {
		if c.IsObjNotFoundErr(err) {
			err = nil
		}
		return false, errors.Wrap(err, "swift check if file exists")
	}
	return true, nil
}

// IsObjNotFoundErr returns true if error means that object is not found. Relevant to Get operations.
func (c *Container) IsObjNotFoundErr(err error) bool {
	return errors.Is(err, swift.ObjectNotFound)
}

// Upload writes the contents of the reader as an object into the container.
func (c *Container) Upload(ctx context.Context, name string, r io.Reader) error {
	size, err := objstore.TryToGetSize(r)
	if err != nil {
		level.Warn(c.logger).Log("msg", "could not guess file size, using large object to avoid issues if the file is larger than limit", "name", name, "err", err)
		// Anything higher or equal to chunk size so the SLO is used.
		size = c.chunkSize
	}
	var file io.WriteCloser
	if size >= c.chunkSize {
		file, err = c.connection.StaticLargeObjectCreateFile(&swift.LargeObjectOpts{
			Container:        c.name,
			ObjectName:       name,
			ChunkSize:        c.chunkSize,
			SegmentContainer: c.segmentsContainer,
			CheckHash:        true,
		})
	} else {
		file, err = c.connection.ObjectCreate(c.name, name, true, "", "", swift.Headers{})
	}
	if err != nil {
		return errors.Wrap(err, "swift failed to create file")
	}
	defer runutil.CloseWithLogOnErr(c.logger, file, "swift upload obj close")
	switch f := file.(type) {
	case io.ReaderFrom:
		if _, err := f.ReadFrom(r); err != nil {
			return errors.Wrap(err, "swift failed to write uploaded file")
		}
	default:
		buf, err := ioutil.ReadAll(r)
		if err != nil {
			return errors.Wrap(err, "swift failed to read uploaded file")
		}
		if _, err = file.Write(buf); err != nil {
			return errors.Wrap(err, "swift failed to write uploaded file")
		}
	}
	return nil
}

// Delete removes the object with the given name.
func (c *Container) Delete(ctx context.Context, name string) error {
	return errors.Wrap(c.connection.LargeObjectDelete(c.name, name), "swift delete object")
}

func (*Container) Close() error {
	// Nothing to close.
	return nil
}

// NewTestContainer creates test objStore client that before returning creates temporary container.
// In a close function it empties and deletes the container.
func NewTestContainer(t testing.TB) (objstore.Bucket, func(), error) {
	config, err := configFromEnv()
	if err != nil {
		return nil, nil, errors.Wrap(err, "swift loading config from ENV")
	}
	if config.ContainerName != "" {
		if os.Getenv("THANOS_ALLOW_EXISTING_BUCKET_USE") == "" {
			return nil, nil, errors.New("OS_CONTAINER_NAME is defined. Normally this tests will create temporary container " +
				"and delete it after test. Unset OS_CONTAINER_NAME env variable to use default logic. If you really want to run " +
				"tests against provided (NOT USED!) container, set THANOS_ALLOW_EXISTING_BUCKET_USE=true. WARNING: That container " +
				"needs to be manually cleared. This means that it is only useful to run one test in a time. This is due " +
				"to safety (accidentally pointing prod container for test) as well as swift not being fully strong consistent.")
		}
		c, err := NewContainerFromConfig(log.NewNopLogger(), config, false)
		if err != nil {
			return nil, nil, errors.Wrap(err, "swift initializing new container")
		}
		if err := c.Iter(context.Background(), "", func(f string) error {
			return errors.Errorf("container %s is not empty", c.Name())
		}); err != nil {
			return nil, nil, errors.Wrapf(err, "swift check container %s", c.Name())
		}
		t.Log("WARNING. Reusing", c.Name(), "container for Swift tests. Manual cleanup afterwards is required")
		return c, func() {}, nil
	}
	config.ContainerName = objstore.CreateTemporaryTestBucketName(t)
	config.SegmentContainerName = config.ContainerName
	c, err := NewContainerFromConfig(log.NewNopLogger(), config, true)
	if err != nil {
		return nil, nil, errors.Wrap(err, "swift initializing new container")
	}
	t.Log("created temporary container for swift tests with name", c.Name())

	return c, func() {
		objstore.EmptyBucket(t, context.Background(), c)
		time.Sleep(time.Second)
		if err := c.connection.ContainerDelete(c.name); err != nil {
			t.Logf("deleting container %s failed: %s", c.Name(), err)
		}
		if err := c.connection.ContainerDelete(c.segmentsContainer); err != nil {
			t.Logf("deleting segments container %s failed: %s", c.segmentsContainer, err)
		}
	}, nil
}
