/*
Copyright 2022 The Flux authors

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

package action

import (
	"errors"

	"github.com/opencontainers/go-digest"
	helmaction "helm.sh/helm/v3/pkg/action"
	helmchart "helm.sh/helm/v3/pkg/chart"
	helmchartutil "helm.sh/helm/v3/pkg/chartutil"
	helmrelease "helm.sh/helm/v3/pkg/release"

	helmdriver "helm.sh/helm/v3/pkg/storage/driver"

	v2 "github.com/fluxcd/helm-controller/api/v2beta2"
	"github.com/fluxcd/helm-controller/internal/chartutil"
	"github.com/fluxcd/helm-controller/internal/release"
)

var (
	ErrReleaseDisappeared = errors.New("release disappeared from storage")
	ErrReleaseNotFound    = errors.New("no release found")
	ErrReleaseNotObserved = errors.New("release not observed to be made for object")
	ErrReleaseDigest      = errors.New("release digest verification error")
	ErrChartChanged       = errors.New("release chart changed")
	ErrConfigDigest       = errors.New("release config values changed")
)

// ReleaseTargetChanged returns true if the given release and/or chart
// name have been mutated in such a way that it no longer has the same release
// target as the Status.Current, by comparing the (storage) namespace, and
// release and chart names. This can be used to e.g. trigger a garbage
// collection of the old release before installing the new one.
func ReleaseTargetChanged(obj *v2.HelmRelease, chartName string) bool {
	cur := obj.GetCurrent()
	switch {
	case obj.Status.StorageNamespace == "", cur == nil:
		return false
	case obj.GetStorageNamespace() != obj.Status.StorageNamespace:
		return true
	case obj.GetReleaseNamespace() != cur.Namespace:
		return true
	case release.ShortenName(obj.GetReleaseName()) != cur.Name:
		return true
	case chartName != cur.ChartName:
		return true
	default:
		return false
	}
}

// LastRelease returns the last release object in the Helm storage with the
// given name.
// It returns an error of type ErrReleaseNotFound if there is no
// release with the given name.
// When the release name is too long, it will be shortened to the maximum
// allowed length using the release.ShortenName function.
func LastRelease(config *helmaction.Configuration, releaseName string) (*helmrelease.Release, error) {
	rls, err := config.Releases.Last(release.ShortenName(releaseName))
	if err != nil {
		if errors.Is(err, helmdriver.ErrReleaseNotFound) {
			return nil, ErrReleaseNotFound
		}
		return nil, err
	}
	return rls, nil
}

// IsInstalled returns true if there is any release in the Helm storage with the
// given name. It returns any error other than driver.ErrReleaseNotFound.
func IsInstalled(config *helmaction.Configuration, releaseName string) (bool, error) {
	_, err := config.Releases.Last(release.ShortenName(releaseName))
	if err != nil {
		if errors.Is(err, helmdriver.ErrReleaseNotFound) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// VerifySnapshot verifies the data of the given v2beta2.Snapshot
// matches the release object in the Helm storage. It returns the verified
// release, or an error of type ErrReleaseNotFound, ErrReleaseDisappeared,
// ErrReleaseDigest or ErrReleaseNotObserved indicating the reason for the
// verification failure.
func VerifySnapshot(config *helmaction.Configuration, snapshot *v2.Snapshot) (rls *helmrelease.Release, err error) {
	if snapshot == nil {
		return nil, ErrReleaseNotFound
	}

	rls, err = config.Releases.Get(snapshot.Name, snapshot.Version)
	if err != nil {
		if errors.Is(err, helmdriver.ErrReleaseNotFound) {
			return nil, ErrReleaseDisappeared
		}
		return nil, err
	}

	if err = VerifyReleaseObject(snapshot, rls); err != nil {
		return nil, err
	}
	return rls, nil
}

// VerifyLastStorageItem verifies the data of the given v2beta2.Snapshot
// matches the last release object in the Helm storage. It returns the release
// and any verification error of type ErrReleaseNotFound, ErrReleaseDisappeared,
// ErrReleaseDigest or ErrReleaseNotObserved indicating the reason for the
// verification failure.
func VerifyLastStorageItem(config *helmaction.Configuration, snapshot *v2.Snapshot) (rls *helmrelease.Release, err error) {
	if snapshot == nil {
		return nil, ErrReleaseNotFound
	}

	rls, err = config.Releases.Last(snapshot.Name)
	if err != nil {
		if errors.Is(err, helmdriver.ErrReleaseNotFound) {
			return nil, ErrReleaseDisappeared
		}
		return nil, err
	}

	if err = VerifyReleaseObject(snapshot, rls); err != nil {
		return nil, err
	}
	return rls, nil
}

// VerifyReleaseObject verifies the data of the given v2beta2.Snapshot
// matches the given Helm release object. It returns an error of type
// ErrReleaseDigest or ErrReleaseNotObserved indicating the reason for the
// verification failure, or nil.
func VerifyReleaseObject(snapshot *v2.Snapshot, rls *helmrelease.Release) error {
	relDig, err := digest.Parse(snapshot.Digest)
	if err != nil {
		return ErrReleaseDigest
	}
	verifier := relDig.Verifier()

	obs := release.ObserveRelease(rls)
	if err = obs.Encode(verifier); err != nil {
		// We are expected to be able to encode valid JSON, error out without a
		// typed error assuming malfunction to signal to e.g. retry.
		return err
	}
	if !verifier.Verified() {
		return ErrReleaseNotObserved
	}
	return nil
}

// VerifyRelease verifies that the data of the given release matches the given
// chart metadata, and the provided values match the Current.ConfigDigest.
// It returns either an error of type ErrReleaseNotFound, ErrChartChanged or
// ErrConfigDigest, or nil.
func VerifyRelease(rls *helmrelease.Release, snapshot *v2.Snapshot, chrt *helmchart.Metadata, vals helmchartutil.Values) error {
	if rls == nil {
		return ErrReleaseNotFound
	}

	if chrt != nil && (rls.Chart.Metadata.Name != chrt.Name || rls.Chart.Metadata.Version != chrt.Version) {
		return ErrChartChanged
	}

	if snapshot == nil || !chartutil.VerifyValues(digest.Digest(snapshot.ConfigDigest), vals) {
		return ErrConfigDigest
	}
	return nil
}
