package services_test

import (
	"os"
	"path"
	"testing"

	"cloudbeast.doni/m/services"
)

const (
	FILE      = "../tests/input/gojo.jpeg"
	OUTPUTDIR = "../tests/output"
)

func TestCompressionService(t *testing.T) {
	data, err := os.ReadFile(FILE)
	if err != nil {
		t.Errorf("The file %s not exist.", FILE)
	}
	compressedData, compressErr := services.CompressData(data)

	if compressErr != nil {
		t.Error("failed to compress data...")
	}

	outputFile, outputErr := os.Create(path.Join(OUTPUTDIR, "output"))
	if outputErr != nil {
		t.Errorf(
			"cannot create a file with %s to the %s dir",
			path.Join(OUTPUTDIR, "output"),
			OUTPUTDIR,
		)
	}

	outputFile.Write(compressedData)
	outputFile.Close()
}

func TestDecompressionService(t *testing.T) {
	data, err := os.ReadFile(path.Join(OUTPUTDIR, "output"))
	if err != nil {
		t.Errorf("The file %s not exist.", path.Join(OUTPUTDIR, "output"))
	}
	decompressedData, decompressErr := services.DecompressData(data)
	if decompressErr != nil {
		t.Error("failed to decompress data...")
	}
	outputFile, outputErr := os.Create(path.Join(OUTPUTDIR, "output2.jpeg"))
	if outputErr != nil {
		t.Errorf(
			"cannot create a file with %s to the %s dir",
			path.Join(OUTPUTDIR, "output2"),
			OUTPUTDIR,
		)
	}
	outputFile.Write(decompressedData)
	outputFile.Close()
}
