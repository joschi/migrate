package awss3

import (
	"context"
	"fmt"
	"go.opentelemetry.io/contrib/instrumentation/github.com/aws/aws-sdk-go-v2/otelaws"
	"io"
	"net/url"
	"os"
	"path"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/golang-migrate/migrate/v4/source"
)

func init() {
	source.Register("s3", &s3Driver{})
}

type S3er interface {
	ListObjectsV2(context.Context, *s3.ListObjectsV2Input, ...func(*s3.Options)) (*s3.ListObjectsV2Output, error)
	GetObject(context.Context, *s3.GetObjectInput, ...func(*s3.Options)) (*s3.GetObjectOutput, error)
}

type s3Driver struct {
	s3client   S3er
	cfg        *Config
	migrations *source.Migrations
}

type Config struct {
	Bucket string
	Prefix string
}

func (s *s3Driver) Open(ctx context.Context, folder string) (source.Driver, error) {
	cfg, err := parseURI(folder)
	if err != nil {
		return nil, err
	}

	awsCfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, err
	}
	// instrument all aws clients
	otelaws.AppendMiddlewares(&awsCfg.APIOptions)

	return WithInstance(ctx, s3.NewFromConfig(awsCfg), cfg)
}

func WithInstance(ctx context.Context, s3client S3er, cfg *Config) (source.Driver, error) {
	driver := &s3Driver{
		cfg:        cfg,
		s3client:   s3client,
		migrations: source.NewMigrations(),
	}

	if err := driver.loadMigrations(ctx); err != nil {
		return nil, err
	}

	return driver, nil
}

func parseURI(uri string) (*Config, error) {
	u, err := url.Parse(uri)
	if err != nil {
		return nil, err
	}

	prefix := strings.Trim(u.Path, "/")
	if prefix != "" {
		prefix += "/"
	}

	return &Config{
		Bucket: u.Host,
		Prefix: prefix,
	}, nil
}

func (s *s3Driver) loadMigrations(ctx context.Context) error {
	output, err := s.s3client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket:    aws.String(s.cfg.Bucket),
		Prefix:    aws.String(s.cfg.Prefix),
		Delimiter: aws.String("/"),
	})
	if err != nil {
		return err
	}

	for _, object := range output.Contents {
		_, fileName := path.Split(aws.ToString(object.Key))

		m, err := source.DefaultParse(fileName)
		if err != nil {
			continue
		}

		if !s.migrations.Append(m) {
			return fmt.Errorf("unable to parse file %v", aws.ToString(object.Key))
		}
	}

	return nil
}

func (s *s3Driver) Close(ctx context.Context) error {
	return nil
}

func (s *s3Driver) First(ctx context.Context) (uint, error) {
	v, ok := s.migrations.First(ctx)
	if !ok {
		return 0, os.ErrNotExist
	}

	return v, nil
}

func (s *s3Driver) Prev(ctx context.Context, version uint) (uint, error) {
	v, ok := s.migrations.Prev(ctx, version)
	if !ok {
		return 0, os.ErrNotExist
	}

	return v, nil
}

func (s *s3Driver) Next(ctx context.Context, version uint) (uint, error) {
	v, ok := s.migrations.Next(ctx, version)
	if !ok {
		return 0, os.ErrNotExist
	}

	return v, nil
}

func (s *s3Driver) ReadUp(ctx context.Context, version uint) (io.ReadCloser, string, error) {
	if m, ok := s.migrations.Up(version); ok {
		return s.open(ctx, m)
	}

	return nil, "", os.ErrNotExist
}

func (s *s3Driver) ReadDown(ctx context.Context, version uint) (io.ReadCloser, string, error) {
	if m, ok := s.migrations.Down(version); ok {
		return s.open(ctx, m)
	}

	return nil, "", os.ErrNotExist
}

func (s *s3Driver) open(ctx context.Context, m *source.Migration) (io.ReadCloser, string, error) {
	key := path.Join(s.cfg.Prefix, m.Raw)
	object, err := s.s3client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.cfg.Bucket),
		Key:    aws.String(key),
	})

	if err != nil {
		return nil, "", err
	}

	return object.Body, m.Identifier, nil
}
