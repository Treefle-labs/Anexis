package services

import (
    "compress/gzip"
    "bytes"
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
    // Code pour décompresser les données
    return data, nil
}
