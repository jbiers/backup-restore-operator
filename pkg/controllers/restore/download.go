package restore

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"strings"

	v1 "github.com/rancher/backup-restore-operator/pkg/apis/resources.cattle.io/v1"
	"github.com/rancher/backup-restore-operator/pkg/objectstore"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apiserver/pkg/storage/value"
)

func (h *handler) downloadFromS3(restore *v1.Restore, objStore *v1.S3ObjectStore, namespace string) (string, error) {
	fmt.Printf("\nobjStore: %v, namespace: %v\n", objStore, namespace)
	s3Client, err := objectstore.GetS3Client(h.ctx, objStore, namespace, h.dynamicClient)
	if err != nil {
		return "", err
	}
	prefix := restore.Spec.BackupFilename
	if len(prefix) == 0 {
		return "", fmt.Errorf("empty backup name")
	}
	folder := objStore.Folder
	if len(folder) != 0 {
		prefix = fmt.Sprintf("%s/%s", folder, prefix)
	}
	targetFileLocation, err := objectstore.DownloadFromS3WithPrefix(s3Client, prefix, objStore.BucketName)
	if err != nil {
		return "", err
	}
	return targetFileLocation, nil
}

// very initial parts: https://medium.com/@skdomino/taring-untaring-files-in-go-6b07cf56bc07
func (h *handler) LoadFromTarGzip(tarGzFilePath string, transformerMap map[schema.GroupResource]value.Transformer) error {
	r, err := os.Open(tarGzFilePath)
	if err != nil {
		return fmt.Errorf("error opening tar.gz backup fike %v", err)
	}

	gz, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	tarball := tar.NewReader(gz)

	for {
		tarContent, err := tarball.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if tarContent.Typeflag != tar.TypeReg {
			continue
		}
		readData, err := ioutil.ReadAll(tarball)
		if err != nil {
			return err
		}
		if strings.Contains(tarContent.Name, "filters") {
			if strings.Contains(tarContent.Name, "filters.json") {
				if err := json.Unmarshal(readData, &h.backupResourceSet); err != nil {
					return fmt.Errorf("error unmarshaling backup filters file: %v", err)
				}
			}
			if strings.Contains(tarContent.Name, "statussubresource.json") {
				if err := json.Unmarshal(readData, &h.resourcesWithStatusSubresource); err != nil {
					return fmt.Errorf("error unmarshaling status subresource info file: %v", err)
				}
			}
			continue
		}

		// tarContent.Name = serviceaccounts.#v1/cattle-system/cattle.json OR users.management.cattle.io#v3/u-lqx8j.json
		err = h.loadDataFromFile(tarContent, readData, transformerMap)
		if err != nil {
			return err
		}
	}
}

func (h *handler) loadDataFromFile(tarContent *tar.Header, readData []byte,
	transformerMap map[schema.GroupResource]value.Transformer) error {
	var name, namespace, additionalAuthenticatedData string

	h.resourcesFromBackup[tarContent.Name] = true
	splitPath := strings.Split(tarContent.Name, "/")
	if len(splitPath) == 2 {
		// cluster scoped resource, since no subdir for namespace
		name = strings.TrimSuffix(splitPath[1], ".json")
		additionalAuthenticatedData = name
	} else {
		// namespaced resource, splitPath[0] =  serviceaccounts.#v1, splitPath[1] = namespace
		name = strings.TrimSuffix(splitPath[2], ".json")
		namespace = splitPath[1]
		additionalAuthenticatedData = fmt.Sprintf("%s#%s", namespace, name)
	}
	gvrStr := splitPath[0]
	gvr := getGVR(gvrStr)

	decryptionTransformer := transformerMap[gvr.GroupResource()]
	if decryptionTransformer != nil {
		var encryptedBytes []byte
		if err := json.Unmarshal(readData, &encryptedBytes); err != nil {
			return err
		}
		decrypted, _, err := decryptionTransformer.TransformFromStorage(encryptedBytes, value.DefaultContext(additionalAuthenticatedData))
		if err != nil {
			return err
		}
		readData = decrypted
	}
	fileMap := make(map[string]interface{})
	err := json.Unmarshal(readData, &fileMap)
	if err != nil {
		return err
	}
	info := objInfo{
		Name:       name,
		GVR:        gvr,
		ConfigPath: tarContent.Name,
	}
	if strings.EqualFold(gvr.Resource, "customresourcedefinitions") {
		h.crdInfoToData[info] = unstructured.Unstructured{Object: fileMap}
	} else {
		if namespace != "" {
			info.Namespace = namespace
			h.namespacedResourceInfoToData[info] = unstructured.Unstructured{Object: fileMap}
		} else {
			h.clusterscopedResourceInfoToData[info] = unstructured.Unstructured{Object: fileMap}
		}
	}
	return nil
}
