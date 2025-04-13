package services

import (
	"bytes"
	"compress/gzip"
	"io"
)

func CompressData(data []byte) ([]byte, error) {
	var b bytes.Buffer

	gz := gzip.NewWriter(&b)
	if _, err := gz.Write(data); err != nil {
		return nil, err
	}

	if err := gz.Close(); err != nil {
		return nil, err
	}

	return b.Bytes(), nil
}

func DecompressData(data []byte) ([]byte, error) {
	b := bytes.NewBuffer(data)

	gz, err := gzip.NewReader(b)
	if err != nil {
		return nil, err
	}

	defer gz.Close()

	decompressed, err := io.ReadAll(gz)
	if err != nil {
		return nil, err
	}

	return decompressed, nil
}
