/*
Copyright 2018 Heptio Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package client

import (
	"archive/tar"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path"
	"path/filepath"

	"github.com/heptio/sonobuoy/pkg/config"
	pluginaggregation "github.com/heptio/sonobuoy/pkg/plugin/aggregation"
	"github.com/pkg/errors"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/remotecommand"
)

var tarCommand = []string{
	"/usr/bin/env",
	"bash",
	"-c",
	fmt.Sprintf("tar cf - %s/*.tar.gz", config.MasterResultsPath),
}

// RetrieveResults copies results from a sonobuoy run into a Reader in tar format.
// It also returns a channel of errors, where any errors encountered when writing results
// will be sent, and an error in the case where the config validation fails.
func (c *SonobuoyClient) RetrieveResults(cfg *RetrieveConfig) (io.Reader, <-chan error, error) {
	if cfg == nil {
		return nil, nil, errors.New("nil RetrieveConfig provided")
	}

	if err := cfg.Validate(); err != nil {
		return nil, nil, errors.Wrap(err, "config validation failed")
	}

	ec := make(chan error, 1)
	client, err := c.Client()
	if err != nil {
		return nil, ec, nil
	}

	// Determine sonobuoy pod name
	podName, err := pluginaggregation.GetStatusPodName(client, cfg.Namespace)
	if err != nil {
		return nil, nil, errors.Wrap(err, "failed to get the name of the aggregator pod to fetch results from")
	}

	restClient := client.CoreV1().RESTClient()
	req := restClient.Post().
		Resource("pods").
		Name(podName).
		Namespace(cfg.Namespace).
		SubResource("exec").
		Param("container", config.MasterContainerName)
	req.VersionedParams(&corev1.PodExecOptions{
		Container: config.MasterContainerName,
		Command:   tarCommand,
		Stdin:     false,
		Stdout:    true,
		Stderr:    false,
	}, scheme.ParameterCodec)
	executor, err := remotecommand.NewSPDYExecutor(c.RestConfig, "POST", req.URL())
	if err != nil {
		ec <- err
		return nil, ec, nil
	}
	reader, writer := io.Pipe()
	go func(writer *io.PipeWriter, ec chan error) {
		defer writer.Close()
		defer close(ec)
		err = executor.Stream(remotecommand.StreamOptions{
			Stdout: writer,
			Tty:    false,
		})
		if err != nil {
			ec <- err
		}
	}(writer, ec)

	return reader, ec, nil
}

/** Everything below this marker originally was copy/pasta'd from k8s/k8s. The modification are:
  exporting UntarAll, returning the list of files created, and the fix for undrained readers  **/

// UntarAll expects a reader that contains tar'd data. It will untar the contents of the reader and write
// the output into destFile under the prefix, prefix. It returns a list of all the
// files it created.
func UntarAll(reader io.Reader, destFile, prefix string) (filenames []string, returnErr error) {
	entrySeq := -1
	filenames = []string{}
	// TODO: use compression here?
	tarReader := tar.NewReader(reader)

	// Ensure the reader gets drained. Tar on some platforms doesn't
	// seem to consume the end-of-archive marker (long string of 0s).
	// If we fail to read them all then any pipes we are connected to
	// may hang.
	defer func() {
		b, err := ioutil.ReadAll(reader)
		if err != nil {
			returnErr = errors.Wrap(err, "failed to drain the tar reader")
			return
		}

		for i := range b {
			if b[i] != 0 {
				returnErr = fmt.Errorf("non-zero data %v read after tar EOF", b[i])
				return
			}
		}
	}()

	for {
		header, err := tarReader.Next()
		if err != nil {
			if err != io.EOF {
				return filenames, err
			}
			break
		}
		entrySeq++
		mode := header.FileInfo().Mode()
		outFileName := path.Join(destFile, header.Name[len(prefix):])
		baseName := path.Dir(outFileName)
		if err := os.MkdirAll(baseName, 0755); err != nil {
			return filenames, err
		}
		if header.FileInfo().IsDir() {
			if err := os.MkdirAll(outFileName, 0755); err != nil {
				return filenames, err
			}
			continue
		}

		// handle coping remote file into local directory
		if entrySeq == 0 && !header.FileInfo().IsDir() {
			exists, err := dirExists(outFileName)
			if err != nil {
				return filenames, err
			}
			if exists {
				outFileName = filepath.Join(outFileName, path.Base(header.Name))
			}
		}

		if mode&os.ModeSymlink != 0 {
			err := os.Symlink(header.Linkname, outFileName)
			if err != nil {
				return filenames, err
			}
		} else {
			outFile, err := os.Create(outFileName)
			if err != nil {
				return filenames, err
			}
			filenames = append(filenames, outFileName)
			defer outFile.Close()
			if _, err := io.Copy(outFile, tarReader); err != nil {
				return filenames, err
			}
			if err := outFile.Close(); err != nil {
				return filenames, err
			}
		}
	}

	if entrySeq == -1 {
		//if no file was copied
		errInfo := fmt.Sprintf("error: %s no such file or directory", prefix)
		return filenames, errors.New(errInfo)
	}
	return filenames, nil
}

// dirExists checks if a path exists and is a directory.
func dirExists(path string) (bool, error) {
	fi, err := os.Stat(path)
	if err == nil && fi.IsDir() {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}
