// SPDX-License-Identifier: AGPL-3.0-only
// Provenance-includes-location: https://github.com/cortexproject/cortex/blob/master/pkg/util/http_test.go
// Provenance-includes-license: Apache-2.0
// Provenance-includes-copyright: The Cortex Authors.

package util_test

import (
	"bytes"
	"context"
	"html/template"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/grafana/mimir/pkg/mimirpb"
	"github.com/grafana/mimir/pkg/util"
	util_log "github.com/grafana/mimir/pkg/util/log"
)

func TestRenderHTTPResponse(t *testing.T) {
	type testStruct struct {
		Name  string `json:"name"`
		Value int    `json:"value"`
	}

	tests := []struct {
		name                string
		headers             map[string]string
		tmpl                string
		expectedOutput      string
		expectedContentType string
		value               testStruct
	}{
		{
			name: "Test Renders json",
			headers: map[string]string{
				"Accept": "application/json",
			},
			tmpl:                "<html></html>",
			expectedOutput:      `{"name":"testName","value":42}`,
			expectedContentType: "application/json",
			value: testStruct{
				Name:  "testName",
				Value: 42,
			},
		},
		{
			name:                "Test Renders html",
			headers:             map[string]string{},
			tmpl:                "<html>{{ .Name }}</html>",
			expectedOutput:      "<html>testName</html>",
			expectedContentType: "text/html; charset=utf-8",
			value: testStruct{
				Name:  "testName",
				Value: 42,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpl := template.Must(template.New("webpage").Parse(tt.tmpl))
			writer := httptest.NewRecorder()
			request := httptest.NewRequest("GET", "/", nil)

			for k, v := range tt.headers {
				request.Header.Add(k, v)
			}

			util.RenderHTTPResponse(writer, tt.value, tmpl, request)

			assert.Equal(t, tt.expectedContentType, writer.Header().Get("Content-Type"))
			assert.Equal(t, 200, writer.Code)
			assert.Equal(t, tt.expectedOutput, writer.Body.String())
		})
	}
}

func TestWriteTextResponse(t *testing.T) {
	w := httptest.NewRecorder()

	util.WriteTextResponse(w, "hello world")

	assert.Equal(t, 200, w.Code)
	assert.Equal(t, "hello world", w.Body.String())
	assert.Equal(t, "text/plain", w.Header().Get("Content-Type"))
}

func TestStreamWriteYAMLResponse(t *testing.T) {
	type testStruct struct {
		Name  string `yaml:"name"`
		Value int    `yaml:"value"`
	}
	tt := struct {
		name                string
		headers             map[string]string
		expectedOutput      string
		expectedContentType string
		value               map[string]*testStruct
	}{
		name: "Test Stream Render YAML",
		headers: map[string]string{
			"Content-Type": "application/yaml",
		},
		expectedContentType: "application/yaml",
		value:               make(map[string]*testStruct),
	}

	// Generate some data to serialize.
	for i := 0; i < rand.Intn(100)+1; i++ {
		ts := testStruct{
			Name:  "testName" + strconv.Itoa(i),
			Value: i,
		}
		tt.value[ts.Name] = &ts
	}
	d, err := yaml.Marshal(tt.value)
	require.NoError(t, err)
	tt.expectedOutput = string(d)
	w := httptest.NewRecorder()

	done := make(chan struct{})
	iter := make(chan interface{})
	go func() {
		util.StreamWriteYAMLResponse(w, iter, util_log.Logger)
		close(done)
	}()
	for k, v := range tt.value {
		iter <- map[string]*testStruct{k: v}
	}
	close(iter)
	<-done
	assert.Equal(t, tt.expectedContentType, w.Header().Get("Content-Type"))
	assert.Equal(t, 200, w.Code)
	assert.YAMLEq(t, tt.expectedOutput, w.Body.String())
}

func TestParseProtoReader(t *testing.T) {
	// 47 bytes compressed and 53 uncompressed
	req := &mimirpb.PreallocWriteRequest{
		WriteRequest: mimirpb.WriteRequest{
			Timeseries: []mimirpb.PreallocTimeseries{
				{
					TimeSeries: &mimirpb.TimeSeries{
						Labels: []mimirpb.LabelAdapter{
							{Name: "foo", Value: "bar"},
						},
						Samples: []mimirpb.Sample{
							{Value: 10, TimestampMs: 1},
							{Value: 20, TimestampMs: 2},
							{Value: 30, TimestampMs: 3},
						},
						Exemplars: []mimirpb.Exemplar{},
					},
				},
			},
		},
	}

	for _, tt := range []struct {
		name           string
		compression    util.CompressionType
		maxSize        int
		expectErr      bool
		useBytesBuffer bool
	}{
		{"rawSnappy", util.RawSnappy, 53, false, false},
		{"noCompression", util.NoCompression, 53, false, false},
		{"gzip", util.Gzip, 53, false, false},
		{"too big rawSnappy", util.RawSnappy, 10, true, false},
		{"too big decoded rawSnappy", util.RawSnappy, 50, true, false},
		{"too big noCompression", util.NoCompression, 10, true, false},
		{"too big gzip", util.Gzip, 10, true, false},
		{"too big decoded gzip", util.Gzip, 50, true, false},

		{"bytesbuffer rawSnappy", util.RawSnappy, 53, false, true},
		{"bytesbuffer noCompression", util.NoCompression, 53, false, true},
		{"bytesbuffer gzip", util.Gzip, 53, false, true},
		{"bytesbuffer too big rawSnappy", util.RawSnappy, 10, true, true},
		{"bytesbuffer too big decoded rawSnappy", util.RawSnappy, 50, true, true},
		{"bytesbuffer too big noCompression", util.NoCompression, 10, true, true},
		{"bytesbuffer too big gzip", util.Gzip, 10, true, true},
		{"bytesbuffer too big decoded gzip", util.Gzip, 50, true, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			require.NoError(t, util.SerializeProtoResponse(w, req, tt.compression))
			var fromWire mimirpb.PreallocWriteRequest

			reader := w.Result().Body
			if tt.useBytesBuffer {
				buf := bytes.Buffer{}
				_, err := buf.ReadFrom(reader)
				require.NoError(t, err)
				reader = bytesBuffered{Buffer: &buf}
			}

			err := util.ParseProtoReader(context.Background(), reader, 0, tt.maxSize, nil, &fromWire, tt.compression)
			if tt.expectErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			fromWire.ClearTimeseriesUnmarshalData() // non-nil unmarshal buffer in PreallocTimeseries breaks equality test
			assert.Equal(t, req, &fromWire)
		})
	}
}

type bytesBuffered struct {
	*bytes.Buffer
}

func (b bytesBuffered) Close() error {
	return nil
}

func (b bytesBuffered) BytesBuffer() *bytes.Buffer {
	return b.Buffer
}

func TestIsRequestBodyTooLargeRegression(t *testing.T) {
	_, err := io.ReadAll(http.MaxBytesReader(httptest.NewRecorder(), io.NopCloser(bytes.NewReader([]byte{1, 2, 3, 4})), 1))
	assert.True(t, util.IsRequestBodyTooLarge(err))
}

func TestNewMsgSizeTooLargeErr(t *testing.T) {
	err := util.MsgSizeTooLargeErr{Actual: 100, Limit: 50}
	msg := `the request has been rejected because its size of 100 bytes exceeds the limit of 50 bytes`

	assert.Equal(t, msg, err.Error())
}

func TestParseRequestFormWithoutConsumingBody(t *testing.T) {
	expected := url.Values{
		"first":  []string{"a", "b"},
		"second": []string{"c"},
	}

	t.Run("GET request", func(t *testing.T) {
		req, err := http.NewRequest("GET", "http://localhost/?"+expected.Encode(), nil)
		require.NoError(t, err)

		actual, err := util.ParseRequestFormWithoutConsumingBody(req)
		require.NoError(t, err)
		assert.Equal(t, expected, actual)

		// Parsing the request again should get the expected values.
		require.NoError(t, req.ParseForm())
		assert.Equal(t, expected, req.Form)
	})

	t.Run("POST request", func(t *testing.T) {
		origBody := newReadCloserObserver(io.NopCloser(strings.NewReader(expected.Encode())))
		req, err := http.NewRequest("POST", "http://localhost/", origBody)
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

		actual, err := util.ParseRequestFormWithoutConsumingBody(req)
		require.NoError(t, err)
		assert.Equal(t, expected, actual)

		// Since the original body has been consumed and discarded, it should have called Close() too.
		assert.True(t, origBody.closeCalled)

		// The request should have been reset to a non-parsed state.
		assert.Nil(t, req.Form)
		assert.Nil(t, req.PostForm)

		// Parsing the request again should get the expected values.
		require.NoError(t, req.ParseForm())
		assert.Equal(t, expected, req.Form)
	})
}
func TestIsValidURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		endpoint string
		valid    bool
	}{
		{
			name:     "valid url",
			endpoint: "https://sts.eu-central-1.amazonaws.com",
			valid:    true,
		},
		{
			name:     "invalid url no scheme",
			endpoint: "sts.eu-central-1.amazonaws.com",
			valid:    false,
		},
		{
			name:     "invalid url invalid scheme setup",
			endpoint: "https:///sts.eu-central-1.amazonaws.com",
			valid:    false,
		},
		{
			name:     "invalid url no host",
			endpoint: "https://",
			valid:    false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			isValid := util.IsValidURL(test.endpoint)
			if test.valid {
				assert.True(t, isValid)
			} else {
				assert.False(t, isValid)
			}
		})
	}
}

type readCloserObserver struct {
	io.ReadCloser
	closeCalled bool
}

func newReadCloserObserver(wrapped io.ReadCloser) *readCloserObserver {
	return &readCloserObserver{
		ReadCloser: wrapped,
	}
}

func (o *readCloserObserver) Close() error {
	o.closeCalled = true
	return o.ReadCloser.Close()
}
