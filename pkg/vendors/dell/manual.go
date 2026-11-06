package dell

import (
	_ "embed"
	"fmt"
	"net/url"
	"os"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

// manualDatabase is the firmware Dell publishes only on the support site,
// which no catalog lists. It ships with the program so a mirror picks these up
// without being told about them; --dell.manual adds to it.
//
//go:embed manual.yaml
var manualDatabase []byte

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
	ComponentIDs []string `yaml:"component_ids"`
	// PCIInfo is the PCI identity the devices are matched on. Older DUPs leave
	// it out of their package.xml, and without it no GUID is published, so the
	// entry can name it here instead.
	PCIInfo  []DellPCIInfo       `yaml:"pci_info"`
	Versions []DellManualVersion `yaml:"versions"`
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

// LoadManual returns the embedded database, then whatever the file at path
// adds to it. A path that names no file is not an error: the embedded entries
// are still published.
//
// An entry in the file replaces the embedded one of the same name, so a mirror
// can pin a version, correct a digest, or drop a version the embedded entry
// lists, without waiting for a release.
func LoadManual(path string) ([]DellManualFirmware, error) {
	entries, err := parseManual(manualDatabase, "embedded")
	if err != nil {
		return nil, err
	}

	if path == "" {
		return entries, nil
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading manual Dell firmware %s: %w", path, err)
	}
	added, err := parseManual(raw, path)
	if err != nil {
		return nil, err
	}

	return mergeManual(entries, added), nil
}

func parseManual(raw []byte, source string) ([]DellManualFirmware, error) {
	var entries []DellManualFirmware
	if err := yaml.Unmarshal(raw, &entries); err != nil {
		return nil, fmt.Errorf("parsing manual Dell firmware %s: %w", source, err)
	}

	for i, entry := range entries {
		if err := entry.validate(); err != nil {
			return nil, fmt.Errorf("manual Dell firmware %s, entry %d: %w", source, i+1, err)
		}
	}

	return entries, nil
}

// mergeManual puts added after base, with an added entry replacing the base
// entry of the same name. Order is kept so the published metadata is stable.
func mergeManual(base, added []DellManualFirmware) []DellManualFirmware {
	out := slices.Clone(base)
	for _, entry := range added {
		replaced := false
		for i := range out {
			if out[i].Name == entry.Name {
				out[i] = entry
				replaced = true
				break
			}
		}
		if !replaced {
			out = append(out, entry)
		}
	}
	return out
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
				ManualPCIInfo: entry.PCIInfo,
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
