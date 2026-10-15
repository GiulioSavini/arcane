package attestations

import (
	"bytes"
	"compress/gzip"
	"testing"

	"github.com/google/go-containerregistry/pkg/v1/static"
	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/require"
)

func TestReadAttestationLayerBytesDecompressesGzip(t *testing.T) {
	statement := []byte(`{"predicateType":"https://slsa.dev/provenance/v0.2"}`)

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	{
		_, err := gz.Write(statement)
		require.NoError(t, err,
			"gzip write: %v", err)
	}
	{

		err := gz.Close()
		require.NoError(t, err,
			"gzip close: %v", err)
	}

	layer := static.NewLayer(buf.Bytes(), inTotoLayerMediaType)
	got, err := readAttestationLayerBytes(layer, "sha256:test")

	require.NoError(t, err,
		"unexpected error: %v", err)

	require.True(t, bytes.Equal(got, statement),
		"expected decompressed statement %q, got %q", statement, got)
}

func TestReadAttestationLayerBytesPassesThroughRawJSON(t *testing.T) {
	statement := []byte(`{"predicateType":"https://spdx.dev/Document"}`)

	layer := static.NewLayer(statement, inTotoLayerMediaType)
	got, err := readAttestationLayerBytes(layer, "sha256:test")

	require.NoError(t, err,
		"unexpected error: %v", err)

	require.True(t, bytes.Equal(got, statement),
		"expected raw statement unchanged %q, got %q", statement, got)
}

func TestReadAttestationLayerBytesDecompressesZstd(t *testing.T) {
	statement := []byte(`{"predicateType":"https://slsa.dev/provenance/v1"}`)

	encoder, err := zstd.NewWriter(nil)

	require.NoError(t, err,
		"zstd writer: %v", err)

	compressed := encoder.EncodeAll(statement, nil)
	{
		closeErr := encoder.Close()
		require.NoError(t, closeErr,
			"zstd close: %v", closeErr)
	}

	layer := static.NewLayer(compressed, inTotoLayerMediaType)
	got, err := readAttestationLayerBytes(layer, "sha256:test")

	require.NoError(t, err,
		"unexpected error: %v", err)

	require.True(t, bytes.Equal(got, statement),
		"expected decompressed statement %q, got %q", statement, got)
}
