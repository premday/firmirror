package dell

import (
	"fmt"
	"net/url"
	"os"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

// DellManualFirmware is one firmware no Dell catalog lists, fetched from the
// URL the entry gives instead. Dell publishes some updates only on the support
// site, keyed by driver ID, so a catalog-driven mirror never sees them.
type DellManualFirmware struct {
	// Name is what a human calls this firmware; it becomes the component summary.
	Name string `yaml:"name"`
	// SystemIDs are the systems the firmware applies to. A run filtered to
	// another system does not download it.
	SystemIDs []string `yaml:"system_ids"`
	// ComponentIDs narrow which devices of the DUP are published, for a DUP
	// that covers several systems. Empty publishes every device it carries.
	ComponentIDs []string            `yaml:"component_ids"`
	Versions     []DellManualVersion `yaml:"versions"`
}

// DellManualVersion is one published build of a manual firmware.
type DellManualVersion struct {
	Version string `yaml:"version"`
	// URL is the direct download, as the support site gives it.
	URL string `yaml:"url"`
	// SHA256 is the digest the support site publishes for the file. It is
	// checked after the download, so a truncated or replaced file is refused.
	SHA256 string `yaml:"sha256"`
	// Size is the published size in bytes, checked alongside the digest.
	Size int64 `yaml:"size"`
}

// LoadManual reads the manual firmware file. A missing path is not an error:
// a mirror that lists none simply reads no manual firmware.
func LoadManual(path string) ([]DellManualFirmware, error) {
	if path == "" {
		return nil, nil
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading manual Dell firmware %s: %w", path, err)
	}

	var entries []DellManualFirmware
	if err := yaml.Unmarshal(raw, &entries); err != nil {
		return nil, fmt.Errorf("parsing manual Dell firmware %s: %w", path, err)
	}

	for i, entry := range entries {
		if err := entry.validate(); err != nil {
			return nil, fmt.Errorf("manual Dell firmware %s, entry %d: %w", path, i+1, err)
		}
	}

	return entries, nil
}

func (m DellManualFirmware) validate() error {
	if strings.TrimSpace(m.Name) == "" {
		return fmt.Errorf("no name")
	}
	if len(m.Versions) == 0 {
		return fmt.Errorf("%s: no versions", m.Name)
	}
	for i, v := range m.Versions {
		if strings.TrimSpace(v.Version) == "" {
			return fmt.Errorf("%s: version %d has no version", m.Name, i+1)
		}
		if v.URL == "" {
			return fmt.Errorf("%s %s: no url", m.Name, v.Version)
		}
		parsed, err := url.Parse(v.URL)
		if err != nil {
			return fmt.Errorf("%s %s: url: %w", m.Name, v.Version, err)
		}
		switch parsed.Scheme {
		case "http", "https", "file":
		default:
			return fmt.Errorf("%s %s: url scheme %q is not http, https or file", m.Name, v.Version, parsed.Scheme)
		}
		if v.SHA256 != "" && len(v.SHA256) != 64 {
			return fmt.Errorf("%s %s: sha256 is not 64 hex characters", m.Name, v.Version)
		}
	}
	return nil
}

// appliesTo reports whether the firmware is for one of the systems the run
// mirrors. A run that filters no system takes everything.
func (m DellManualFirmware) appliesTo(systemIDs []string) bool {
	if len(systemIDs) == 0 {
		return true
	}
	for _, id := range m.SystemIDs {
		if slices.Contains(systemIDs, id) {
			return true
		}
	}
	return false
}

// components turns the manual entries into catalog components, so the rest of
// the vendor reads them the way it reads a catalog entry: one component per
// version, carrying the URL to download and the digest to check it against.
func (dv *DellVendor) manualComponents() []DellSoftwareComponent {
	var out []DellSoftwareComponent

	for _, entry := range dv.Manual {
		if !entry.appliesTo(dv.SystemIDs) {
			continue
		}
		for _, version := range entry.Versions {
			out = append(out, DellSoftwareComponent{
				Path:          version.URL,
				ManualURL:     version.URL,
				ManualDevices: entry.ComponentIDs,
				VendorVersion: version.Version,
				PackageType:   "LLXP",
				ComponentType: DellTranslatableWithValue{Value: "FRMW"},
				Name:          DellTranslatable{Display: []DellTranslatableEntry{{Lang: "en", Value: entry.Name}}},
				Description:   DellTranslatable{Display: []DellTranslatableEntry{{Lang: "en", Value: entry.Name}}},
				SupportedSystems: []DellBrand{{
					Key: 3,
					DellTranslatable: DellTranslatable{
						Display: []DellTranslatableEntry{{Lang: "en", Value: "Enterprise Servers"}},
					},
					Models: manualModels(entry.SystemIDs),
				}},
				Hash:          version.SHA256,
				HashAlgorithm: "SHA256",
				Size:          version.Size,
			})
		}
	}

	return out
}

func manualModels(systemIDs []string) []DellModel {
	models := make([]DellModel, 0, len(systemIDs))
	for _, id := range systemIDs {
		models = append(models, DellModel{SystemID: id})
	}
	return models
}
