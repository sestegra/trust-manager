/*
Copyright 2023 The cert-manager Authors.

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

package main

import (
	"archive/tar"
	"bytes"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/match"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/google/go-containerregistry/pkg/v1/types"
	"github.com/spf13/cobra"
)

var CommandAppendLayers = cobra.Command{
	Use:   "append-layers oci-path [path-to-tarball...]",
	Short: "Appends a tarball or directory to every image in an OCI index.",
	Args:  cobra.MinimumNArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		oci := args[0]
		extra := args[1:]

		if len(extra) == 0 {
			return
		}

		path, err := layout.FromPath(oci)
		must("could not load oci directory", err)

		index, err := path.ImageIndex()
		must("could not load oci image index", err)

		indexMediaType, err := index.MediaType()
		must("could not get image index media type", err)

		layerType := types.DockerLayer
		if indexMediaType == types.OCIImageIndex {
			layerType = types.OCILayer
		}

		layers := []v1.Layer{}
		for _, path := range extra {
			layers = append(layers, loadLayerFromDirOrTarball(path, layerType))
		}

		index = appendLayersToImageIndex(index, layers)

		_, err = layout.Write(oci, index)
		must("could not write image", err)
	},
}

func loadLayerFromDirOrTarball(path string, mediaType types.MediaType) v1.Layer {
	stat, err := os.Stat(path)
	must("could not open directory or tarball", err)

	var layer v1.Layer
	if stat.IsDir() {
		var buf bytes.Buffer

		tw := tar.NewWriter(&buf)

		filepath.Walk(path, func(target string, info fs.FileInfo, err error) error {
			must("walk error", err)

			header, err := tar.FileInfoHeader(info, info.Name())
			must("could not create tar header", err)

			name, err := filepath.Rel(path, target)
			must("could not build relative path", err)

			// Write simplified header, this removes all fields that would cause
			// the build to be non-reproducible (like modtime for example)
			err = tw.WriteHeader(&tar.Header{
				Typeflag: header.Typeflag,
				Name:     name,
				Mode:     header.Mode,
				Linkname: header.Linkname,
				Size:     header.Size,
			})

			must("could not write tar header", err)

			if !info.IsDir() {
				file, err := os.Open(target)
				must("could not write tar contents", err)

				defer file.Close()

				_, err = io.Copy(tw, file)
				must("could not write tar contents", err)
			}

			return nil
		})

		tw.Close()

		byts := buf.Bytes()

		layer, err = tarball.LayerFromOpener(
			func() (io.ReadCloser, error) {
				return io.NopCloser(bytes.NewReader(byts)), nil
			},
			tarball.WithMediaType(mediaType),
		)

	} else {
		layer, err = tarball.LayerFromFile(
			path,
			tarball.WithMediaType(mediaType),
		)
	}

	must("could not open directory or tarball", err)
	return layer
}

func appendLayersToImageIndex(index v1.ImageIndex, layers []v1.Layer) v1.ImageIndex {
	manifest, err := index.IndexManifest()
	must("could not load oci image manifest", err)

	for _, descriptor := range manifest.Manifests {
		switch {
		case descriptor.MediaType.IsImage():
			slog.Info("found image", "digest", descriptor.Digest, "platform", descriptor.Platform)

			img, err := index.Image(descriptor.Digest)
			must("could not load oci image with digest", err)

			img, err = mutate.AppendLayers(img, layers...)
			must("could not load append layer to image", err)

			digest, err := img.Digest()
			must("could not get image digest", err)

			size, err := img.Size()
			must("could not get image size", err)

			slog.Info("appended layers to image", "old_digest", descriptor.Digest, "digest", digest, "platform", descriptor.Platform)

			index = mutate.RemoveManifests(index, match.Digests(descriptor.Digest))

			descriptor.Digest = digest
			descriptor.Size = size
			index = mutate.AppendManifests(index, mutate.IndexAddendum{
				Add:        img,
				Descriptor: descriptor,
			})

		case descriptor.MediaType.IsIndex():
			slog.Info("found image index", "digest", descriptor.Digest)

			child, err := index.ImageIndex(descriptor.Digest)
			must("could not load oci index manifest", err)

			child = appendLayersToImageIndex(child, layers)

			digest, err := child.Digest()
			must("could not get index digest", err)

			size, err := child.Size()
			must("could not get index size", err)

			index = mutate.RemoveManifests(index, match.Digests(descriptor.Digest))

			descriptor.Digest = digest
			descriptor.Size = size
			index = mutate.AppendManifests(index, mutate.IndexAddendum{
				Add:        child,
				Descriptor: descriptor,
			})
		}
	}

	return index
}
