package awsutil

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDynamicBucketRegion(t *testing.T) {
	defaultConfig, err := Session()
	require.NoError(t, err)
	defaultRegion := defaultConfig.Region

	testCases := []struct {
		name                 string
		inputS3URL           string
		expectedBucketRegion string
		locationResponse     string
		headRegion           string
	}{
		{
			name:                 "bucket location is returned by S3",
			inputS3URL:           "s3://test-bucket",
			expectedBucketRegion: "eu-central-1",
			locationResponse:     "eu-central-1",
		},
		{
			name:                 "bucket location works when the URL contains a key",
			inputS3URL:           "s3://test-bucket/charts/chart-0.1.2.tgz",
			expectedBucketRegion: "eu-central-1",
			locationResponse:     "eu-central-1",
		},
		{
			name:                 "head bucket header is used as fallback",
			inputS3URL:           "s3://test-bucket",
			expectedBucketRegion: "eu-west-1",
			headRegion:           "eu-west-1",
		},
		{
			name:                 "invalid URL does not change the default region",
			inputS3URL:           "://not/a/URL",
			expectedBucketRegion: defaultRegion,
		},
		{
			name:                 "empty URL does not change the default region",
			inputS3URL:           "",
			expectedBucketRegion: defaultRegion,
		},
		{
			name:                 "missing bucket region does not change the default region",
			inputS3URL:           "s3://" + uuid.NewString(),
			expectedBucketRegion: defaultRegion,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			var requests int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				atomic.AddInt32(&requests, 1)
				if r.Method == http.MethodGet && r.URL.Query().Has("location") && tc.locationResponse != "" {
					w.Header().Set("Content-Type", "application/xml")
					w.WriteHeader(http.StatusOK)
					_, _ = w.Write([]byte(`<LocationConstraint xmlns="http://s3.amazonaws.com/doc/2006-03-01/">` + tc.locationResponse + `</LocationConstraint>`))
					return
				}
				if r.Method == http.MethodGet && r.URL.Query().Has("location") {
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				if r.Method == http.MethodHead && tc.headRegion != "" {
					w.Header().Set("X-Amz-Bucket-Region", tc.headRegion)
				}
				w.WriteHeader(http.StatusNotFound)
			}))
			defer server.Close()

			serverURL, err := url.Parse(server.URL)
			require.NoError(t, err)
			client := &http.Client{Transport: &rewritingTransport{
				target: serverURL,
				base:   http.DefaultTransport,
			}}

			actualConfig, err := Session(
				withHTTPClient(client),
				withRetryMaxAttempts(1),
				DynamicBucketRegion(tc.inputS3URL),
			)
			require.NoError(t, err)
			assert.Equal(t, tc.expectedBucketRegion, actualConfig.Region)
			if tc.inputS3URL == "://not/a/URL" || tc.inputS3URL == "" {
				assert.Zero(t, atomic.LoadInt32(&requests))
			}
		})
	}
}

func TestSessionWithCustomEndpoint(t *testing.T) {
	t.Setenv("AWS_ENDPOINT", "http://foobar:1234")
	t.Setenv("AWS_DISABLE_SSL", "true")
	t.Setenv("HELM_S3_REGION", "us-west-2")

	cfg, err := Session()
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	// Note: In AWS SDK v2, endpoint configuration is validated differently
	// The endpoint resolver is checked when making actual API calls
	if cfg.Region != "us-west-2" {
		t.Fatalf("Expected to set us-west-2 region, got %s", cfg.Region)
	}
}

func TestDynamicBucketRegionDisabled(t *testing.T) {
	var requestsReceived int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requestsReceived, 1)
		w.Header().Set("X-Amz-Bucket-Region", "us-west-2")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	serverURL, err := url.Parse(server.URL)
	require.NoError(t, err)
	client := &http.Client{Transport: &rewritingTransport{target: serverURL, base: http.DefaultTransport}}

	t.Setenv("HELM_S3_DYNAMIC_REGION_ENABLED", "false")

	defaultCfg, err := Session()
	require.NoError(t, err)
	defaultRegion := defaultCfg.Region

	actualCfg, err := Session(withHTTPClient(client), DynamicBucketRegion("s3://test-bucket"))
	require.NoError(t, err)

	assert.Equal(t, defaultRegion, actualCfg.Region)
	assert.Zero(t, atomic.LoadInt32(&requestsReceived),
		"expected no HTTP requests when dynamic region discovery is disabled")
}

func TestSessionWithInvalidEndpoint(t *testing.T) {
	t.Setenv("AWS_ENDPOINT", "foobar:1234")
	t.Setenv("AWS_DISABLE_SSL", "true")
	t.Setenv("HELM_S3_REGION", "us-west-2")

	_, err := Session()
	if err == nil {
		t.Fatalf("Expected error for endpoint without scheme, got nil")
	}
	if err.Error() != "endpoint must include a scheme (e.g., https://)" {
		t.Fatalf("Expected 'endpoint must include a scheme' error, got: %v", err)
	}
}

// rewritingTransport routes every request to target, regardless of the
// request's original scheme/host. Used in tests to point the AWS SDK at an
// httptest.Server.
type rewritingTransport struct {
	target *url.URL
	base   http.RoundTripper
}

func (t *rewritingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.URL.Scheme = t.target.Scheme
	req.URL.Host = t.target.Host
	req.Host = t.target.Host
	return t.base.RoundTrip(req)
}

func withHTTPClient(client *http.Client) SessionOption {
	return func(options *config.LoadOptions) error {
		return config.WithHTTPClient(client)(options)
	}
}

func withRetryMaxAttempts(attempts int) SessionOption {
	return func(options *config.LoadOptions) error {
		return config.WithRetryMaxAttempts(attempts)(options)
	}
}
