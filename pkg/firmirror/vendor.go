package firmirror

import (
	"context"

	"github.com/premday/firmirror/pkg/lvfs"
)

type Vendor interface {
	// FetchCatalog retrieves the catalog of firmware for the vendor.
	FetchCatalog(ctx context.Context) (Catalog, error)
	// RetrieveFirmware downloads the firmware file for the given firmware entry to tmpDir.
	// For vendors like HPE, this step is required before processing.
	RetrieveFirmware(ctx context.Context, entry FirmwareEntry, tmpDir string) error
}

// Catalog represents a generic catalog of firmware entries.
type Catalog interface {
	// ListEntries returns all firmware entries in the catalog.
	ListEntries() []FirmwareEntry
}

// FirmwareEntry represents a single firmware entry in the catalog.
type FirmwareEntry interface {
	// GetFilename will be used to determine if the firmware has already been downloaded
	// and if it should be processed
	GetFilename() string
	// GetSourceURL returns the original download URL for this firmware
	GetSourceURL() string
	// ToAppstream converts this firmware entry to AppStream components.
	// A single firmware entry may produce multiple components (e.g. one per supported device).
	ToAppstream() ([]lvfs.Component, error)
}

// Payload is one firmware file in a cabinet and the components that flash it.
type Payload struct {
	File       string
	Components []*lvfs.Component
}

// PayloadEntry is a FirmwareEntry that publishes several firmware files in one
// cabinet, each flashing a different device. components is what ToAppstream
// returned for the same entry.
type PayloadEntry interface {
	CabinetPayloads(components []*lvfs.Component) []Payload
}
