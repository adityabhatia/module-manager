package util

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	yamlUtil "k8s.io/apimachinery/pkg/util/yaml"

	"github.com/kyma-project/module-manager/pkg/types"

	"github.com/pkg/errors"
	"helm.sh/helm/v3/pkg/kube"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/cli-runtime/pkg/resource"
	"sigs.k8s.io/yaml"
)

const (
	ManifestDir                     = "manifest"
	manifestFile                    = "manifest.yaml"
	configFileName                  = "installConfig.yaml"
	YamlDecodeBufferSize            = 2048
	OthersReadExecuteFilePermission = 0o755
	DebugLogLevel                   = 2
	TraceLogLevel                   = 3
)

func GetNamespaceObjBytes(clientNs string) ([]byte, error) {
	namespace := v1.Namespace{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "Namespace",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: clientNs,
			Labels: map[string]string{
				"name": clientNs,
			},
		},
	}
	return yaml.Marshal(namespace)
}

func FilterExistingResources(resources kube.ResourceList) (kube.ResourceList, error) {
	existingResources := kube.ResourceList{}
	errs := make([]error, 0, len(resources))
	for i := range resources {
		info := resources[i]
		helper := resource.NewHelper(info.Client, info.Mapping)
		_, err := helper.Get(info.Namespace, info.Name)
		if err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			errs = append(errs, errors.Wrapf(err,
				"could not get information about the resource %s / %s", info.Name, info.Namespace))
			continue
		}

		// TODO: Adapt standard labels / annotations here

		existingResources.Append(info)
	}

	if len(errs) > 0 {
		return existingResources, types.NewMultiError(errs)
	}

	return existingResources, nil
}

func CleanFilePathJoin(root, destDir string) (string, error) {
	// On Windows, this is a drive separator. On UNIX-like, this is the path list separator.
	// In neither case do we want to trust a TAR that contains these.
	if strings.Contains(destDir, ":") {
		return "", errors.New("path contains ':', which is illegal")
	}

	// The Go tar library does not convert separators for us.
	// We assume here, as we do elsewhere, that `\\` means a Windows path.
	destDir = strings.ReplaceAll(destDir, "\\", "/")

	// We want to alert the user that something bad was attempted. Cleaning it
	// is not a good practice.
	for _, part := range strings.Split(destDir, "/") {
		if part == ".." {
			return "", errors.New("path contains '..', which is illegal")
		}
	}

	// If a path is absolute, the creator of the TAR is doing something shady.
	if path.IsAbs(destDir) {
		return "", errors.New("path is absolute, which is illegal")
	}

	newPath := filepath.Join(root, filepath.Clean(destDir))

	return filepath.ToSlash(newPath), nil
}

func ParseManifestStringToObjects(manifest string) (*types.ManifestResources, error) {
	objects := &types.ManifestResources{}
	reader := yamlUtil.NewYAMLReader(bufio.NewReader(strings.NewReader(manifest)))
	for {
		rawBytes, err := reader.Read()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return objects, nil
			}

			return nil, fmt.Errorf("invalid YAML doc: %w", err)
		}

		rawBytes = bytes.TrimSpace(rawBytes)
		unstructuredObj := unstructured.Unstructured{}
		if err := yaml.Unmarshal(rawBytes, &unstructuredObj); err != nil {
			objects.Blobs = append(objects.Blobs, append(bytes.TrimPrefix(rawBytes, []byte("---\n")), '\n'))
		}

		if len(rawBytes) == 0 || bytes.Equal(rawBytes, []byte("null")) || len(unstructuredObj.Object) == 0 {
			continue
		}

		objects.Items = append(objects.Items, &unstructuredObj)
	}
}

func GetFsChartPath(imageSpec types.ImageSpec) string {
	return filepath.Join(os.TempDir(), fmt.Sprintf("%s-%s", imageSpec.Name, imageSpec.Ref))
}

func GetConfigFilePath(config types.ImageSpec) string {
	return filepath.Join(os.TempDir(), filepath.Join(config.Ref, configFileName))
}

func GetFsManifestChartPath(imageChartPath string) string {
	return filepath.Join(imageChartPath, ManifestDir, manifestFile)
}

func GetYamlFileContent(filePath string) (interface{}, error) {
	var fileContent interface{}
	file, err := os.Open(filePath)
	if err != nil {
		return nil, err
	}
	if file != nil {
		if err = yamlUtil.NewYAMLOrJSONDecoder(file, YamlDecodeBufferSize).Decode(&fileContent); err != nil {
			return nil, fmt.Errorf("reading content from file path %s: %w", filePath, err)
		}
		err = file.Close()
	}

	return fileContent, err
}

func WriteToFile(filePath string, bytes []byte) error {
	// create directory
	if err := os.MkdirAll(filepath.Dir(filePath), fs.ModePerm); err != nil {
		return err
	}

	// create file
	file, err := os.Create(filePath)
	if err != nil {
		return fmt.Errorf("file creation at path %s caused an error: %w", filePath, err)
	}

	// write to file
	if _, err = file.Write(bytes); err != nil {
		return fmt.Errorf("writing file to path %s caused an error: %w", filePath, err)
	}
	return file.Close()
}

func GetResourceLabel(resource client.Object, labelName string) (string, error) {
	labels := resource.GetLabels()
	label, ok := labels[labelName]
	if !ok {
		return "", &types.LabelNotFoundError{
			Resource:  resource,
			LabelName: label,
		}
	}
	return label, nil
}

func GetStringifiedYamlFromFilePath(filePath string) (string, error) {
	file, err := os.ReadFile(filePath)
	if err != nil {
		return "", err
	}

	return string(file), err
}

// CalculateHash returns hash for interfaceToBeHashed.
func CalculateHash(interfaceToBeHashed any) (uint32, error) {
	data, err := json.Marshal(interfaceToBeHashed)
	if err != nil {
		return 0, err
	}

	h := fnv.New32a()
	h.Write(data)
	return h.Sum32(), nil
}

// Transform applies the passed object transformations to the manifest string passed.
func Transform(ctx context.Context, manifestStringified string,
	object types.BaseCustomObject, transforms []types.ObjectTransform,
) (*types.ManifestResources, error) {
	objects, err := ParseManifestStringToObjects(manifestStringified)
	if err != nil {
		return nil, err
	}

	for _, transform := range transforms {
		if err = transform(ctx, object, objects); err != nil {
			return nil, err
		}
	}

	return objects, nil
}
