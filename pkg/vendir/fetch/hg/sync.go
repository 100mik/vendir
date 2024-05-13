// Copyright 2024 The Carvel Authors.
// SPDX-License-Identifier: Apache-2.0

package hg

import (
	"fmt"
	"io"
	"os"
	"strings"

	ctlconf "carvel.dev/vendir/pkg/vendir/config"
	ctlfetch "carvel.dev/vendir/pkg/vendir/fetch"
	ctlcache "carvel.dev/vendir/pkg/vendir/fetch/cache"
)

type Sync struct {
	opts       ctlconf.DirectoryContentsHg
	log        io.Writer
	refFetcher ctlfetch.RefFetcher
	cache      ctlcache.Cache
}

func NewSync(opts ctlconf.DirectoryContentsHg,
	log io.Writer, refFetcher ctlfetch.RefFetcher, cache ctlcache.Cache) Sync {

	return Sync{opts, log, refFetcher, cache}
}

func (d Sync) Desc() string {
	ref := "?"
	switch {
	case len(d.opts.Ref) > 0:
		ref = d.opts.Ref
	}
	return fmt.Sprintf("%s@%s", d.opts.URL, ref)
}

func (d Sync) Sync(dstPath string, tempArea ctlfetch.TempArea) (ctlconf.LockDirectoryContentsHg, error) {
	hgLockConf := ctlconf.LockDirectoryContentsHg{}

	incomingTmpPath, err := tempArea.NewTempDir("hg")
	if err != nil {
		return hgLockConf, err
	}

	defer os.RemoveAll(incomingTmpPath)

	hg, err := NewHg(d.opts, d.log, d.refFetcher, tempArea)
	if err != nil {
		return hgLockConf, fmt.Errorf("Setting up hg: %w", err)
	}
	defer hg.Close()

	if cachePath, ok := d.cache.Has("hg", hg.CacheID()); ok {
		// fetch from cachedDir
		if err := d.cache.CopyFrom("hg", hg.CacheID(), incomingTmpPath); err != nil {
			return hgLockConf, fmt.Errorf("Extracting cached hg clone: %w", err)
		}
		// Sync if needed
		if !hg.CloneHasTargetRef(cachePath) {
			if err := hg.SyncClone(incomingTmpPath); err != nil {
				return hgLockConf, fmt.Errorf("Syncing hg repository: %w", err)
			}
			if err := d.cache.Save("hg", hg.CacheID(), incomingTmpPath); err != nil {
				return hgLockConf, fmt.Errorf("Saving hg repository to cache: %w", err)
			}
		}
	} else {
		// fetch in the target directory
		if err := hg.Clone(incomingTmpPath); err != nil {
			return hgLockConf, fmt.Errorf("Cloning hg repository: %w", err)
		}
		if err := d.cache.Save("hg", hg.CacheID(), incomingTmpPath); err != nil {
			return hgLockConf, fmt.Errorf("Saving hg repository to cache: %w", err)
		}
	}

	// now checkout the wanted revision
	info, err := hg.Checkout(incomingTmpPath)
	if err != nil {
		return hgLockConf, fmt.Errorf("Checking out hg repository: %s", err)
	}

	hgLockConf.SHA = info.SHA
	hgLockConf.ChangeSetTitle = d.singleLineChangeSetTitle(info.ChangeSetTitle)

	err = os.RemoveAll(dstPath)
	if err != nil {
		return hgLockConf, fmt.Errorf("Deleting dir %s: %s", dstPath, err)
	}

	err = os.Rename(incomingTmpPath, dstPath)
	if err != nil {
		return hgLockConf, fmt.Errorf("Moving directory '%s' to staging dir: %s", incomingTmpPath, err)
	}

	return hgLockConf, nil
}

func (Sync) singleLineChangeSetTitle(in string) string {
	pieces := strings.SplitN(in, "\n", 2)
	if len(pieces) > 1 {
		return pieces[0] + "..."
	}
	return pieces[0]
}
