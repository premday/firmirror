package dell

import (
	"compress/gzip"
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"hash"
	"html"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/premday/firmirror/pkg/firmirror"
	"github.com/premday/firmirror/pkg/lvfs"
	"github.com/premday/firmirror/pkg/utils"
	"golang.org/x/text/encoding/unicode"
	"golang.org/x/text/transform"
)

// Catalog component types the Redfish update service can flash.
var updatableComponentTypes = []string{"FRMW", "BIOS"}

// NewDellVendor creates a new Dell vendor instance reading the catalog and the
// firmware from baseURL.
//
// baseURL is a copy of that site: an HTTP(S) URL, a file:// URL, or a plain
// directory, so an rsync mirror can be read where it sits. The published
// metadata is unaffected either way, because it points at the location the
// catalog itself names.
func NewDellVendor(systemIDs []string, baseURL string) *DellVendor {
	vendor := &DellVendor{
		BaseURL:   strings.TrimSuffix(baseURL, "/"),
		SystemIDs: systemIDs,
	}

	return vendor
}

func (dv *DellVendor) FetchCatalog(ctx context.Context) (firmirror.Catalog, error) {
	catalog, err := dv.fetchCatalog(ctx)
	if err != nil {
		return nil, err
	}

	// Filter catalog entries based on vendor settings
	filteredCatalog := dv.filterCatalog(catalog)
	return filteredCatalog, nil
}

func (dv *DellVendor) fetchCatalog(ctx context.Context) (*DellCatalog, error) {
	catalogBody, err := utils.DownloadFile(ctx, dv.BaseURL+"/catalog/catalog.xml.gz")
	if err != nil {
		return nil, fmt.Errorf("downloading Dell catalog: %w", err)
	}
	defer catalogBody.Close()

	rawCatalog, err := gzip.NewReader(catalogBody)
	if err != nil {
		return nil, fmt.Errorf("decompressing Dell catalog: %w", err)
	}
	defer rawCatalog.Close()

	return parseDellCatalog(rawCatalog)
}

func parseDellCatalog(r io.Reader) (*DellCatalog, error) {
	// The XML decoder only reads UTF-8, so we need to convert the UTF-16 to UTF-8
	unicodeReader := transform.NewReader(r, unicode.BOMOverride(unicode.UTF16(unicode.LittleEndian, unicode.IgnoreBOM).NewDecoder()))

	var dellCatalog DellCatalog

	xmlDecoder := xml.NewDecoder(unicodeReader)
	xmlDecoder.CharsetReader = func(charset string, input io.Reader) (io.Reader, error) {
		// But then we need to ignore the charset in the XML declaration
		// because the XML decoder will still try to read it as UTF-8
		// and fail if it's not
		return input, nil
	}
	if err := xmlDecoder.Decode(&dellCatalog); err != nil {
		return nil, fmt.Errorf("parsing Dell catalog XML: %w", err)
	}

	return &dellCatalog, nil
}

func (dv *DellVendor) filterCatalog(catalog *DellCatalog) *DellCatalog {
	filteredComponents := []DellSoftwareComponent{}

	for _, fw := range catalog.SoftwareComponents {
		// The BIOS is its own component type, so FRMW alone drops every BIOS
		// update. Drivers (DRVR) and applications (APAC) are not flashable.
		if !slices.Contains(updatableComponentTypes, fw.ComponentType.Value) {
			continue
		}

		// If no SystemIDs filter is set, include all firmware
		if len(dv.SystemIDs) == 0 {
			filteredComponents = append(filteredComponents, fw)
			continue
		}

	systemLoop:
		for _, system := range fw.SupportedSystems {
			for _, model := range system.Models {
				if slices.Contains(dv.SystemIDs, model.SystemID) {
					filteredComponents = append(filteredComponents, fw)
					break systemLoop
				}
			}
		}
	}

	filteredCatalog := *catalog // Copy the catalog
	filteredCatalog.SoftwareComponents = filteredComponents
	return &filteredCatalog
}

func (dv *DellVendor) RetrieveFirmware(ctx context.Context, entry firmirror.FirmwareEntry, tmpDir string) error {
	dellEntry, ok := entry.(*DellFirmwareEntry)
	if !ok {
		return fmt.Errorf("invalid entry type for Dell vendor")
	}

	fwPath := dellEntry.DellSoftwareComponent.Path
	filepath := filepath.Join(tmpDir, filepath.Base(fwPath))
	if _, err := os.Stat(filepath); os.IsNotExist(err) {
		if err := utils.DownloadFileToDest(ctx, dv.BaseURL+"/"+fwPath, filepath); err != nil {
			return fmt.Errorf("downloading Dell firmware %s: %w", fwPath, err)
		}
	}

	if err := verifyDownload(filepath, dellEntry.DellSoftwareComponent); err != nil {
		os.Remove(filepath)
		return fmt.Errorf("verifying Dell firmware %s: %w", fwPath, err)
	}

	return nil
}

// verifyDownload checks the downloaded file against the size and the digest the
// catalog gives, so a truncated or corrupted download is never published. The
// SHA-256 is preferred over the MD5; a component the catalog gives neither for
// is only checked for its size.
func verifyDownload(path string, component *DellSoftwareComponent) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()

	var hasher hash.Hash
	var want string
	switch {
	case component.Hash != "" && strings.EqualFold(component.HashAlgorithm, "SHA256"):
		hasher, want = sha256.New(), component.Hash
	case component.HashMD5 != "":
		hasher, want = md5.New(), component.HashMD5
	default:
		slog.Warn("The catalog gives no digest for this firmware, so it is not verified", "path", component.Path)
		hasher = sha256.New()
	}

	size, err := io.Copy(hasher, file)
	if err != nil {
		return err
	}
	if component.Size > 0 && size != component.Size {
		return fmt.Errorf("downloaded %d bytes, the catalog says %d", size, component.Size)
	}
	if got := hex.EncodeToString(hasher.Sum(nil)); want != "" && !strings.EqualFold(got, want) {
		return fmt.Errorf("digest is %s, the catalog says %s", got, want)
	}
	return nil
}

func (dc *DellCatalog) ListEntries() []firmirror.FirmwareEntry {
	entries := []firmirror.FirmwareEntry{}
	for _, fw := range dc.SoftwareComponents {
		entries = append(entries, &DellFirmwareEntry{
			Filename:              filepath.Base(fw.Path),
			DellSoftwareComponent: &fw,
			// The catalog names the location Dell publishes on, which is
			// where a host reading the mirror can read about the firmware.
			SourceURL: dc.BaseLocation + "/" + fw.Path,
		})
	}
	return entries
}

func (dfe *DellFirmwareEntry) GetFilename() string {
	return dfe.Filename
}

func (dfe *DellFirmwareEntry) GetSourceURL() string {
	return dfe.SourceURL
}

func (dfe *DellFirmwareEntry) ToAppstream() ([]lvfs.Component, error) {
	return processFirmware(*dfe.DellSoftwareComponent)
}

func processFirmware(fw DellSoftwareComponent) ([]lvfs.Component, error) {
	fwName, err := getString(fw.Name, "en")
	if err != nil {
		return nil, fmt.Errorf("failed to get firmware name: %w", err)
	}

	description, err := getString(fw.Description, "en")
	if err != nil {
		return nil, err
	}

	var rebootCustom []lvfs.Custom
	if fw.RebootRequired {
		rebootMessage, err := getString(fw.ImportantInfo, "en")
		if err != nil {
			rebootMessage = "Reboot is required for the update to take effect."
		}
		rebootCustom = []lvfs.Custom{
			{Key: "LVFS::DeviceFlags", Value: "skips-restart"},
			{Key: "LVFS::UpdateMessage", Value: rebootMessage},
		}
	}

	var categories []string
	switch fw.LUCategory.Value {
	case "BIOS":
		categories = append(categories, "X-System")
	case "Serial ATA", "SAS Drive":
		categories = append(categories, "X-Drive")
	case "Express Flash PCIe SSD":
		categories = append(categories, "X-SolidStateDrive")
	case "Network":
		categories = append(categories, "X-NetworkInterface")
	case "Chassis System Management":
		categories = append(categories, "X-Controller")
	case "iDRAC with Lifecycle Controller":
		categories = append(categories, "X-BaseboardManagementController")
	}

	// Part of the component identity, see componentUUID.
	systemIDs := supportedSystemIDs(fw)

	// Create one component per supported device
	var components []lvfs.Component
	for _, dev := range fw.SupportedDevices {
		out := lvfs.Component{
			Type:            "firmware",
			MetadataLicense: "proprietary",
			ProjectLicense:  "proprietary",
		}

		devName, err := getString(dev.DellTranslatable, "en")
		if err != nil {
			return nil, fmt.Errorf("failed to get device name for component %s: %w", dev.ComponentID, err)
		}
		out.Name = devName
		out.Summary = fwName
		out.Description = lvfs.Description{
			Value: "<p>" + html.EscapeString(description) + "</p>",
		}
		out.DeveloperName = "Dell"
		out.ID = fmt.Sprintf("com.%s.%s", strings.ToLower("Dell"), componentUUID(dev.ComponentID, systemIDs))

		// Provides: GUIDs for this specific device across all system IDs
		for _, systemID := range systemIDs {
			out.Provides = append(out.Provides, lvfs.Firmware{
				Type: "flashed",
				Text: uuid.NewSHA1(uuid.NameSpaceDNS, fmt.Appendf(nil, "REDFISH\\VENDOR_Dell&SYSTEMID_%s&SOFTWAREID_%s", systemID, dev.ComponentID)).String(),
			})
		}

		out.Custom = append(out.Custom, rebootCustom...)

		out.Categories = append(out.Categories, categories...)

		out.Releases = append(out.Releases, lvfs.Release{
			Version:     fw.VendorVersion,
			Date:        fw.DateTime.Format(time.DateOnly),
			Timestamp:   unixTime(fw.DateTime),
			Description: out.Description,
			Urgency:     getUrgency(fw.Criticality.Value),
		})

		out.Custom = append(out.Custom, lvfs.Custom{
			Key:   "LVFS::UpdateProtocol",
			Value: "org.dmtf.redfish",
		}, lvfs.Custom{
			Key: "LVFS::DeviceIntegrity",
			// All Dell firmware going through Redfish are signed
			Value: "signed",
		})

		components = append(components, out)
	}

	return components, nil
}

// unixTime is the Unix time of a date the catalog gave, or 0 when it gave none.
func unixTime(date time.Time) int64 {
	if date.IsZero() {
		return 0
	}
	return date.Unix()
}

// supportedSystemIDs returns the sorted, de-duplicated systems an entry covers.
func supportedSystemIDs(fw DellSoftwareComponent) []string {
	var systemIDs []string
	for _, brand := range fw.SupportedSystems {
		for _, system := range brand.Models {
			systemIDs = append(systemIDs, system.SystemID)
		}
	}
	slices.Sort(systemIDs)
	return slices.Compact(systemIDs)
}

// componentUUID keys a component on the device and the systems it applies to.
// Dell ships one package per platform family for the same device; sharing an ID
// would merge their releases and offer a machine a payload it cannot flash.
func componentUUID(deviceComponentID string, systemIDs []string) string {
	seed := deviceComponentID
	if len(systemIDs) > 0 {
		seed = fmt.Sprintf("%s&SYSTEMIDS_%s", deviceComponentID, strings.Join(systemIDs, ","))
	}
	return uuid.NewSHA1(uuid.NameSpaceDNS, []byte(seed)).String()
}

func getString(strings DellTranslatable, language string) (string, error) {
	for _, l := range strings.Display {
		if l.Lang == language {
			return l.Value, nil
		}
	}
	return "", fmt.Errorf("language not found: %s", language)
}

func getUrgency(criticality int64) string {
	switch criticality {
	case 1:
		return "medium"
	case 2:
		return "critical"
	case 3:
		return "low"
	default:
		return "medium"
	}
}
