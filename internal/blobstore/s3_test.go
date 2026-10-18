package blobstore

import (
	"fmt"
	"net/http/httptest"
	"net/url"
	"os"
	"sync/atomic"
	"testing"

	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"
)

var bucketN atomic.Int64

// TestS3_Conformance runs the shared suite against an in-process S3 fake, so
// it always runs, and against a real S3-API store (Garage, SeaweedFS) when
// NOTIFY_TEST_S3_ENDPOINT, _ACCESS_KEY, _SECRET_KEY and _BUCKET are set.
func TestS3_Conformance(t *testing.T) {
	t.Run("fake", func(t *testing.T) {
		backend := s3mem.New()
		srv := httptest.NewServer(gofakes3.New(backend).Server())
		t.Cleanup(srv.Close)
		u, _ := url.Parse(srv.URL)
		conformance(t, func(t *testing.T) Store {
			bucket := fmt.Sprintf("bucket-%d", bucketN.Add(1))
			if err := backend.CreateBucket(bucket); err != nil {
				t.Fatal(err)
			}
			s, err := NewS3(S3Config{Endpoint: u.Host, Region: "garage", Bucket: bucket, Prefix: t.Name(), AccessKey: "k", SecretKey: "s"})
			if err != nil {
				t.Fatal(err)
			}
			return s
		})
	})
	endpoint := os.Getenv("NOTIFY_TEST_S3_ENDPOINT")
	if endpoint == "" {
		return
	}
	t.Run("real", func(t *testing.T) {
		conformance(t, func(t *testing.T) Store {
			s, err := NewS3(S3Config{
				Endpoint: endpoint, Region: os.Getenv("NOTIFY_TEST_S3_REGION"), Bucket: os.Getenv("NOTIFY_TEST_S3_BUCKET"),
				Prefix:    "notify-test/" + t.Name(),
				AccessKey: os.Getenv("NOTIFY_TEST_S3_ACCESS_KEY"), SecretKey: os.Getenv("NOTIFY_TEST_S3_SECRET_KEY"),
				UseTLS: os.Getenv("NOTIFY_TEST_S3_TLS") == "1",
			})
			if err != nil {
				t.Fatal(err)
			}
			return s
		})
	})
}
