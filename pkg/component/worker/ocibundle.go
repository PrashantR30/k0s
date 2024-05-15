/*
Copyright 2021 k0s authors

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

package worker

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/avast/retry-go"
	"github.com/containerd/containerd"
	"github.com/containerd/containerd/platforms"
	"github.com/fsnotify/fsnotify"
	"github.com/sirupsen/logrus"

	"github.com/k0sproject/k0s/internal/pkg/dir"
	"github.com/k0sproject/k0s/pkg/component/manager"
	"github.com/k0sproject/k0s/pkg/component/prober"
	"github.com/k0sproject/k0s/pkg/config"
	"github.com/k0sproject/k0s/pkg/constant"
	"github.com/k0sproject/k0s/pkg/debounce"
)

// OCIBundleReconciler tries to import OCI bundle into the running containerd instance
type OCIBundleReconciler struct {
	k0sVars *config.CfgVars
	log     *logrus.Entry
	loaded  map[string]time.Time
	*prober.EventEmitter
}

var _ manager.Component = (*OCIBundleReconciler)(nil)

// NewOCIBundleReconciler builds new reconciler
func NewOCIBundleReconciler(vars *config.CfgVars) *OCIBundleReconciler {
	return &OCIBundleReconciler{
		k0sVars:      vars,
		log:          logrus.WithField("component", "OCIBundleReconciler"),
		EventEmitter: prober.NewEventEmitter(),
		loaded:       make(map[string]time.Time),
	}
}

func (a *OCIBundleReconciler) Init(_ context.Context) error {
	return dir.Init(a.k0sVars.OCIBundleDir, constant.ManifestsDirMode)
}

// load loads all OCI bundle files into containerd. Read all files from the oci bundle
// directory and loads them only once. If the file is already loaded and hasn't changed
// it will skip it. Errors are logged but not returned, upon failure in one file this
// function logs the error and moves to the next file.
func (a *OCIBundleReconciler) load(ctx context.Context) {
	files, err := os.ReadDir(a.k0sVars.OCIBundleDir)
	if err != nil {
		a.log.WithError(err).Errorf("can't read bundles directory")
		a.Emit("can't read bundles directory")
		return
	}

	a.EmitWithPayload("importing OCI bundles", files)
	if len(files) == 0 {
		return
	}

	var client *containerd.Client
	sock := filepath.Join(a.k0sVars.RunDir, "containerd.sock")
	if err := retry.Do(func() error {
		client, err = containerd.New(
			sock,
			containerd.WithDefaultNamespace("k8s.io"),
			containerd.WithDefaultPlatform(platforms.OnlyStrict(platforms.DefaultSpec())),
		)
		if err != nil {
			a.log.WithError(err).Errorf("can't connect to containerd socket %s", sock)
			return err
		}

		if _, err := client.ListImages(ctx); err != nil {
			a.log.WithError(err).Errorf("can't use containerd client")
			return err
		}
		return nil
	}, retry.Context(ctx), retry.Delay(time.Second*5)); err != nil {
		payload := map[string]interface{}{"socket": sock, "error": err}
		a.EmitWithPayload("can't connect to containerd socket", payload)
		return
	}
	defer client.Close()

	for _, file := range files {
		payload := map[string]interface{}{"file": file.Name(), "error": err}
		fpath := filepath.Join(a.k0sVars.OCIBundleDir, file.Name())
		finfo, err := os.Stat(fpath)
		if err != nil {
			a.log.WithError(err).Errorf("can't stat file %s", fpath)
			a.EmitWithPayload("can't stat file", payload)
			continue
		}

		if when, ok := a.loaded[file.Name()]; ok && when.Equal(finfo.ModTime()) {
			continue
		}

		if err := a.unpackBundle(ctx, client, fpath); err != nil {
			a.EmitWithPayload("unpacking OCI bundle error", payload)
			a.log.WithError(err).Errorf("can't unpack bundle %s", file.Name())
			continue
		}

		// if succeed in loading the bundle, remember the time.
		a.loaded[file.Name()] = finfo.ModTime()
		a.EmitWithPayload("unpacked OCI bundle", file.Name())
	}
	a.Emit("finished importing OCI bundle")
}

// watch creates a fs watched on the oci bundle directory. This function calls load() anytime
// a new file is created on the directory or a write operation took place . Events are debounced
// with a timeout of 10 seconds. This function is blocking.
func (a *OCIBundleReconciler) watch(ctx context.Context) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		a.log.WithError(err).Error("failed to create watcher for OCI bundles")
		return
	}
	defer watcher.Close()

	if err := watcher.Add(a.k0sVars.OCIBundleDir); err != nil {
		a.log.WithError(err).Error("failed to watch for OCI bundles")
		return
	}

	debouncer := debounce.Debouncer[fsnotify.Event]{
		Input:   watcher.Events,
		Timeout: 10 * time.Second,
		Filter: func(item fsnotify.Event) bool {
			switch item.Op {
			case fsnotify.Create, fsnotify.Write:
				return true
			default:
				return false
			}
		},
		Callback: func(fsnotify.Event) {
			a.log.Info("OCI bundle directory changed, reconciling")
			a.load(ctx)
		},
	}

	go func() {
		for {
			err, ok := <-watcher.Errors
			if !ok {
				return
			}
			a.log.WithError(err).Error("error while watching oci bundle directory")
		}
	}()

	a.log.Infof("started to watch events on %s", a.k0sVars.OCIBundleDir)
	if err := debouncer.Run(ctx); err != nil {
		a.log.WithError(err).Warn("oci bundle watch bouncer exited with error")
	}
}

// Starts initiate the OCI bundle loader. It does an initial load of the directory and
// once it is done, it starts a watcher on its own goroutine.
func (a *OCIBundleReconciler) Start(ctx context.Context) error {
	a.load(ctx)
	go a.watch(ctx)
	return nil
}

func (a OCIBundleReconciler) unpackBundle(ctx context.Context, client *containerd.Client, bundlePath string) error {
	r, err := os.Open(bundlePath)
	if err != nil {
		return fmt.Errorf("can't open bundle file %s: %v", bundlePath, err)
	}
	defer r.Close()
	images, err := client.Import(ctx, r)
	if err != nil {
		return fmt.Errorf("can't import bundle: %v", err)
	}
	is := client.ImageService()
	for _, i := range images {
		a.log.Infof("Imported image %s", i.Name)
		// Update labels for each image to include io.cri-containerd.pinned=pinned
		fieldpaths := []string{"labels.io.cri-containerd.pinned"}
		if i.Labels == nil {
			i.Labels = make(map[string]string)
		}
		i.Labels["io.cri-containerd.pinned"] = "pinned"
		_, err := is.Update(ctx, i, fieldpaths...)
		if err != nil {
			return fmt.Errorf("failed to add io.cri-containerd.pinned label for image %s: %w", i.Name, err)
		}
	}
	return nil
}

func (a *OCIBundleReconciler) Stop() error {
	return nil
}
