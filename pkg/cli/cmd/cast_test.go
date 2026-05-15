package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestParseCastFilesResources(t *testing.T) {
	// Create a temporary directory
	tempDir, err := os.MkdirTemp("", "cast-test")
	assert.NoError(t, err)
	defer os.RemoveAll(tempDir)

	// Create a valid service YAML file
	validServiceYAML := `
service:
  name: test-service
  namespace: default
  image: nginx:latest
  scale: 1
  ports:
    - name: http
      port: 80
`
	validFilePath := filepath.Join(tempDir, "valid-service.yaml")
	err = os.WriteFile(validFilePath, []byte(validServiceYAML), 0644)
	assert.NoError(t, err)

	// Create an invalid service YAML file (missing required fields)
	invalidServiceYAML := `
service:
  name: invalid-service
  # Missing required image field
  scale: 1
`
	invalidFilePath := filepath.Join(tempDir, "invalid-service.yaml")
	err = os.WriteFile(invalidFilePath, []byte(invalidServiceYAML), 0644)
	assert.NoError(t, err)

	// Create a YAML file with multiple services
	multiServiceYAML := `
services:
  - name: service-1
    image: image-1
    scale: 1
  - name: service-2
    image: image-2
    scale: 2
`
	multiFilePath := filepath.Join(tempDir, "multi-service.yaml")
	err = os.WriteFile(multiFilePath, []byte(multiServiceYAML), 0644)
	assert.NoError(t, err)

	// Test loading a valid service file
	t.Run("LoadValidServiceFile", func(t *testing.T) {
		opts := &castOptions{}
		info, err := parseCastFilesResources([]string{validFilePath}, []string{}, opts, nil)
		assert.NoError(t, err)
		assert.Len(t, info.ServicesByFile, 1)
		assert.Len(t, info.ServicesByFile[validFilePath], 1)
		assert.Equal(t, "test-service", info.ServicesByFile[validFilePath][0].Name)
		assert.Equal(t, "default", info.ServicesByFile[validFilePath][0].Namespace)
		assert.Equal(t, "nginx:latest", info.ServicesByFile[validFilePath][0].Image)
		assert.Equal(t, 1, info.ServicesByFile[validFilePath][0].Scale)
	})

	// Test loading an invalid service file
	t.Run("LoadInvalidServiceFile", func(t *testing.T) {
		_, err := parseCastFilesResources([]string{invalidFilePath}, []string{}, &castOptions{}, nil)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "validation failed")
	})

	// Test loading a file with multiple services
	t.Run("LoadMultiServiceFile", func(t *testing.T) {
		opts := &castOptions{}
		opts.namespace = "default"
		info, err := parseCastFilesResources([]string{multiFilePath}, []string{}, opts, nil)
		assert.NoError(t, err)
		assert.Len(t, info.ServicesByFile, 1)
		assert.Len(t, info.ServicesByFile[multiFilePath], 2)
		assert.Equal(t, "service-1", info.ServicesByFile[multiFilePath][0].Name)
		assert.Equal(t, "service-2", info.ServicesByFile[multiFilePath][1].Name)
	})

	// Test loading multiple files
	t.Run("LoadMultipleFiles", func(t *testing.T) {
		opts := &castOptions{}
		opts.namespace = "default"
		info, err := parseCastFilesResources([]string{validFilePath, multiFilePath}, []string{}, opts, nil)
		assert.NoError(t, err)
		assert.Len(t, info.ServicesByFile, 2)
		assert.Len(t, info.ServicesByFile[validFilePath], 1)
		assert.Len(t, info.ServicesByFile[multiFilePath], 2)
	})
}
