package firmirror

import (
	"fmt"
	"net/url"
	"path"
	"strings"

	"github.com/premday/firmirror/pkg/lvfs"
)

// validateFirmwareBaseURL accepts an empty value, or an absolute HTTP(S) URL.
func validateFirmwareBaseURL(baseURL string) error {
	if baseURL == "" {
		return nil
	}
	u, err := url.Parse(baseURL)
	if err != nil {
		return fmt.Errorf("invalid firmware base URL %q: %w", baseURL, err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("invalid firmware base URL %q: want an absolute http(s) URL", baseURL)
	}
	return nil
}

// bareLocation reduces a published location to the name the package is stored
// under. Packages are keyed by that name, so everything that compares
// locations (superseding, cleaning up) works on it rather than on the URL.
func bareLocation(location string) string {
	if !strings.Contains(location, "://") {
		return location
	}
	u, err := url.Parse(location)
	if err != nil {
		return location
	}
	return path.Base(u.Path)
}

// publishedLocation is the location written to the metadata. fwupd hands it to
// curl before re-basing it on a remote's FirmwareBaseURI, so a bare name only
// works for remotes that don't set one.
func (f *FirmirrorSyncer) publishedLocation(location string) string {
	if location == "" || f.Config.FirmwareBaseURL == "" {
		return location
	}
	return strings.TrimRight(f.Config.FirmwareBaseURL, "/") + "/" + location
}

// rewriteLocations returns a copy of components with every location passed
// through fn, and whether any of them changed. The input is left untouched.
func rewriteLocations(components *lvfs.Components, fn func(string) string) (*lvfs.Components, bool) {
	changed := false
	apply := func(location string) string {
		out := fn(location)
		if out != location {
			changed = true
		}
		return out
	}

	out := *components
	out.Component = make([]lvfs.Component, len(components.Component))
	for i, component := range components.Component {
		component.Releases = make([]lvfs.Release, len(component.Releases))
		for j, release := range components.Component[i].Releases {
			release.Location = apply(release.Location)
			release.Artifacts = make([]lvfs.Artifact, len(release.Artifacts))
			for k, artifact := range components.Component[i].Releases[j].Artifacts {
				artifact.Location = apply(artifact.Location)
				release.Artifacts[k] = artifact
			}
			component.Releases[j] = release
		}
		out.Component[i] = component
	}
	return &out, changed
}
