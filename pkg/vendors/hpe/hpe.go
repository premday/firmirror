package hpe

import (
	"archive/zip"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/premday/firmirror/pkg/firmirror"
	"github.com/premday/firmirror/pkg/lvfs"
	"github.com/premday/firmirror/pkg/utils"
)

// DefaultBaseURL is the public HPE SDR repository holding one fwpp-<gen>
// repository per server generation.
const DefaultBaseURL = "https://downloads.linux.hpe.com/SDR/repo"

// NewHPEVendor creates a new HPE vendor instance reading repo from baseURL.
//
// baseURL is a copy of SDR/repo: an HTTP(S) URL, a file:// URL, or a plain
// directory, so an rsync mirror can be read where it sits. Whichever it is, the
// published metadata keeps pointing at the public repository, because that is
// where a host reading the mirror can go to read about the firmware.
func NewHPEVendor(repo, baseURL string) *HPEVendor {
	return &HPEVendor{
		BaseURL:     strings.TrimSuffix(baseURL, "/") + "/" + repo,
		UpstreamURL: DefaultBaseURL + "/" + repo,
	}
}

// FetchCatalog implements the Vendor interface
func (hv *HPEVendor) FetchCatalog(ctx context.Context) (firmirror.Catalog, error) {
	catalog, err := hv.fetchCatalog(ctx)
	if err != nil {
		return nil, err
	}

	// Filter catalog entries based on vendor settings
	filteredCatalog := hv.filterCatalog(catalog)
	return filteredCatalog, nil
}

func (hv *HPEVendor) fetchCatalog(ctx context.Context) (*HPECatalog, error) {
	indexurl := hv.BaseURL + "/current/fwrepodata/fwrepo.json"
	jsondata, err := utils.DownloadFile(ctx, indexurl)
	if err != nil {
		return nil, fmt.Errorf("downloading HPE catalog from %s: %w", indexurl, err)
	}
	defer jsondata.Close()

	catalog, err := parseHPECatalog(jsondata)
	if err != nil {
		return nil, err
	}
	// A vendor built by hand, with no upstream of its own, publishes the
	// location it read the firmware from.
	catalog.UpstreamURL = hv.UpstreamURL
	if catalog.UpstreamURL == "" {
		catalog.UpstreamURL = hv.BaseURL
	}
	return catalog, nil
}

func parseHPECatalog(r io.Reader) (*HPECatalog, error) {
	var entries map[string]HPECatalogEntry
	if err := json.NewDecoder(r).Decode(&entries); err != nil {
		return nil, fmt.Errorf("decoding HPE catalog JSON: %w", err)
	}

	return &HPECatalog{Entries: entries}, nil
}

func (hv *HPEVendor) filterCatalog(catalog *HPECatalog) *HPECatalog {
	filteredEntries := make(map[string]HPECatalogEntry)

	for filename, entry := range catalog.Entries {
		// Only include entries that end with .fwpkg
		if strings.HasSuffix(filename, ".fwpkg") {
			filteredEntries[filename] = entry
		}
	}

	filteredCatalog := &HPECatalog{
		Entries:     filteredEntries,
		UpstreamURL: catalog.UpstreamURL,
	}
	return filteredCatalog
}

// RetrieveFirmware implements the Vendor interface
func (hv *HPEVendor) RetrieveFirmware(ctx context.Context, entry firmirror.FirmwareEntry, tmpDir string) error {
	hpeEntry, ok := entry.(*HPEFirmwareEntry)
	if !ok {
		return fmt.Errorf("invalid entry type for HPE vendor")
	}

	// Try to fetch the sidecar .json metadata (available in some repos, e.g. gen12)
	jsonURL := hv.BaseURL + "/current/" + strings.TrimSuffix(hpeEntry.Filename, ".fwpkg") + ".json"
	if resp, err := utils.DownloadFile(ctx, jsonURL); err == nil {
		data, readErr := io.ReadAll(resp)
		resp.Close()
		if readErr != nil {
			slog.Warn("Failed to read payload JSON sidecar", "url", jsonURL, "error", readErr)
		} else {
			hpeEntry.PayloadJSON = data
		}
	}

	filepath := filepath.Join(tmpDir, filepath.Base(hpeEntry.Filename))
	if _, err := os.Stat(filepath); os.IsNotExist(err) {
		if err := utils.DownloadFileToDest(ctx, hv.BaseURL+"/current/"+hpeEntry.Filename, filepath); err != nil {
			return fmt.Errorf("downloading HPE firmware %s: %w", hpeEntry.Filename, err)
		}
	}

	// Store the download path in the entry for later processing
	hpeEntry.DownloadPath = filepath
	return nil
}

// ListEntries implements the Catalog interface for HPECatalog
func (hc *HPECatalog) ListEntries() []firmirror.FirmwareEntry {
	entries := []firmirror.FirmwareEntry{}
	for filename, catalogEntry := range hc.Entries {
		entry := catalogEntry // Create a copy to avoid pointer issues
		entries = append(entries, &HPEFirmwareEntry{
			Filename:  filename,
			Entry:     &entry,
			SourceURL: hc.UpstreamURL + "/current/" + filename,
		})
	}
	return entries
}

// GetFilename implements the FirmwareEntry interface
func (hfe *HPEFirmwareEntry) GetFilename() string {
	return hfe.Filename
}

// GetSourceURL implements the FirmwareEntry interface
func (hfe *HPEFirmwareEntry) GetSourceURL() string {
	return hfe.SourceURL
}

// ToAppstream implements the FirmwareEntry interface
// HPE requires the firmware to be downloaded first, so we use the stored path
func (hfe *HPEFirmwareEntry) ToAppstream() ([]lvfs.Component, error) {
	if hfe.DownloadPath == "" {
		return nil, fmt.Errorf("firmware must be retrieved first using RetrieveFirmware")
	}

	// Prefer sidecar .json metadata if available (richer than in-package payload.json)
	payloadFile := hfe.PayloadJSON
	if payloadFile == nil {
		var err error
		payloadFile, err = readFileFromZip(hfe.DownloadPath, "payload.json")
		if err != nil {
			return nil, err
		}
	}

	var payload HPEPayload
	if err := json.Unmarshal(payloadFile, &payload); err != nil {
		return nil, err
	}

	appstream, err := buildAppStream(payload, hfe.Entry)
	if err != nil {
		return nil, err
	}

	return []lvfs.Component{*appstream}, nil
}

// buildAppStream converts an HPE firmware payload to an AppStream component.
// The catalogEntry provides fallback metadata for older firmware packages
// whose payload.json lacks the "package" section.
// Note: we make the assumption that all devices in the payload will have the same version
// as well as the install duration.
func buildAppStream(fw HPEPayload, catalogEntry *HPECatalogEntry) (*lvfs.Component, error) {
	out := lvfs.Component{
		Type:            "firmware",
		MetadataLicense: "proprietary",
		ProjectLicense:  "proprietary",
	}

	var devices []string
	for _, dev := range fw.Devices.Device {
		devices = append(devices, dev.DeviceName)
		// TODO:properly create GUID
		// deviceclass ?
		out.Provides = append(out.Provides, lvfs.Firmware{
			Type: "flashed",
			Text: dev.Target,
		})
	}
	slices.Sort(devices)
	devices = slices.Compact(devices)
	out.Name = strings.Join(devices[:], "/")

	manufacturer, err := getString(fw.Package.ManufacturerName, "en")
	if err != nil {
		manufacturer = "Hewlett Packard Enterprise"
	}
	out.DeveloperName = manufacturer

	if len(fw.Package.SwKeys) > 0 {
		out.ID = fmt.Sprintf("com.%s.%s", strings.ToLower(strings.ReplaceAll(manufacturer, " ", "")), strings.ReplaceAll(fw.Package.SwKeys[0].Name, " ", ""))
	} else {
		out.ID = fmt.Sprintf("com.%s.%s", strings.ToLower(strings.ReplaceAll(manufacturer, " ", "")), fw.DeviceClass)
	}

	if fw.Package.Installation.RebootRequired == "yes" {
		out.Custom = append(out.Custom, lvfs.Custom{
			Key:   "LVFS::DeviceFlags",
			Value: "skips-restart",
		})
		rebootMessage, err := getString(fw.Package.Installation.RebootDetails[0].Language, "en")
		if err != nil {
			return nil, err
		}

		out.Custom = append(out.Custom, lvfs.Custom{
			Key:   "LVFS::UpdateMessage",
			Value: rebootMessage,
		})
	}

	summary, err := getString(fw.Package.Name, "en")
	if err != nil {
		summary = out.Name
	}
	out.Summary = strings.ReplaceAll(strings.ReplaceAll(summary, "\t", ""), "  ", " ")

	description, err := getString(fw.Package.Description, "en")
	if err != nil && catalogEntry != nil {
		description = catalogEntry.Description
	}
	if description != "" {
		out.Description = lvfs.Description{
			Value: "<p>" + html.EscapeString(description) + "</p>",
		}
	}

	var releaseDate time.Time
	if fw.Package.ReleaseDate != "" {
		releaseDate, err = time.Parse("2006-01-02T15:04:05", fw.Package.ReleaseDate)
		if err != nil {
			return nil, err
		}
	} else if catalogEntry != nil && catalogEntry.Date != "" {
		releaseDate, err = time.Parse("20060102", catalogEntry.Date)
		if err != nil {
			return nil, fmt.Errorf("failed to parse catalog date %q: %w", catalogEntry.Date, err)
		}
	}

	out.Releases = append(out.Releases, lvfs.Release{
		Version:         fw.Devices.Device[0].Version,
		Date:            releaseDate.Format(time.DateOnly),
		InstallDuration: fw.Devices.Device[0].FirmwareImages[0].InstallDurationSec,
		Description:     out.Description,
	})

	for _, category := range fw.Package.Category {
		switch category.Key {
		case "2900095":
			// Firmware - Network
			out.Categories = append(out.Categories, "X-NetworkInterface")
		case "2900213":
			// Firmware - iLO
			out.Categories = append(out.Categories, "X-BaseboardManagementController")
		}
	}

	out.Custom = append(out.Custom, lvfs.Custom{
		Key:   "LVFS::UpdateProtocol",
		Value: "org.dmtf.redfish",
	}, lvfs.Custom{
		Key: "LVFS::DeviceIntegrity",
		// All fwpkg going through Redfish are signed
		Value: "signed",
	})

	return &out, nil
}

func getString(translations []HPETranslations, language string) (string, error) {
	for _, l := range translations {
		if l.Lang == language {
			return l.XLate, nil
		}
	}
	if len(translations) > 0 {
		return translations[0].XLate, nil
	}
	return "", fmt.Errorf("no translations available")
}

func readFileFromZip(zipFile, filename string) ([]byte, error) {
	archive, err := zip.OpenReader(zipFile)
	if err != nil {
		return nil, fmt.Errorf("opening zip %s: %w", zipFile, err)
	}
	defer archive.Close()

	for _, f := range archive.File {
		if f.Name == filename {
			reader, err := f.Open()
			if err != nil {
				return nil, fmt.Errorf("opening %s in zip: %w", filename, err)
			}
			defer reader.Close()

			data, err := io.ReadAll(reader)
			if err != nil {
				return nil, fmt.Errorf("reading %s from zip: %w", filename, err)
			}
			return data, nil
		}
	}
	return nil, fmt.Errorf("file not found in zip: %s", filename)
}
