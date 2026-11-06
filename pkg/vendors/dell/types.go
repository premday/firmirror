package dell

import (
	"encoding/xml"
	"time"
)

// DellVendor implements the Vendor interface for Dell
type DellVendor struct {
	BaseURL string
	// SystemIDs filters which system to include. If nil or empty, includes all systems. Example: ["0C60"]
	SystemIDs []string
	// Manual is firmware no Dell catalog lists, read from the file the run
	// names. It is published alongside the catalog's own entries.
	Manual []DellManualFirmware
}

// DellCatalog represents the catalog element of a Dell catalog
type DellCatalog struct {
	XMLName            xml.Name                `xml:"Manifest"`
	Version            string                  `xml:"version,attr"`
	DateTime           time.Time               `xml:"dateTime,attr"`
	BaseLocation       string                  `xml:"baseLocation,attr"`
	SoftwareBundle     []DellSoftwareBundle    `xml:"SoftwareBundle"`
	SoftwareComponents []DellSoftwareComponent `xml:"SoftwareComponent"`
}

// DellFirmwareEntry implements the FirmwareEntry interface for Dell
type DellFirmwareEntry struct {
	Filename              string
	DellSoftwareComponent *DellSoftwareComponent
	SourceURL             string
	// Payloads is set by RetrieveFirmware for a DUP, which carries one firmware
	// image per supported device. Nil for a plain catalog payload.
	Payloads []DellPayload
	// DUPPackage is the DUP's own package.xml, set alongside Payloads.
	DUPPackage *DellDUPPackage
}

// DellPayload is one firmware image a DUP carries, and the device it flashes.
type DellPayload struct {
	File        string // basename inside the work directory and inside the CAB
	ComponentID string // from package.xml <Device componentID=…>
	PCIInfo     []DellPCIInfo
}

// DellSoftwareComponent represents a software component like firmware or driver
type DellSoftwareComponent struct {
	DateTime      time.Time `xml:"dateTime,attr,omitempty"`
	DellVersion   string    `xml:"dellVersion,attr"`
	Hash          string    `xml:"hash,attr,omitempty"`
	HashAlgorithm string    `xml:"hashAlgorithm,attr,omitempty"`
	HashMD5       string    `xml:"hashMD5,attr,omitempty"`
	PackageID     string    `xml:"packageID,attr"`
	PackageType   string    `xml:"packageType,attr"`
	Path          string    `xml:"path,attr"`
	// ManualURL is set for a firmware no catalog lists, whose Path is the
	// direct download the manual file gives rather than a catalog-relative one.
	ManualURL string `xml:"-"`
	// ManualDevices narrows which devices of a manual DUP are published. Empty
	// publishes every device the DUP carries.
	ManualDevices []string `xml:"-"`
	// ManualPCIInfo is the PCI identity a manual entry names, for a DUP whose
	// package.xml leaves it out.
	ManualPCIInfo          []DellPCIInfo               `xml:"-"`
	RebootRequired         bool                        `xml:"rebootRequired,attr"`
	ReleaseDate            string                      `xml:"releaseDate,attr,omitempty"`
	ReleaseID              string                      `xml:"releaseID,attr,omitempty"`
	SchemaVersion          string                      `xml:"schemaVersion,attr"`
	Size                   int64                       `xml:"size,attr,omitempty"`
	VendorVersion          string                      `xml:"vendorVersion,attr"`
	Name                   DellTranslatable            `xml:"Name"`
	ComponentType          DellTranslatableWithValue   `xml:"ComponentType"`
	Description            DellTranslatable            `xml:"Description"`
	LUCategory             DellTranslatableWithValue   `xml:"LUCategory"`
	Category               DellTranslatableWithValue   `xml:"Category"`
	ImportantInfo          DellTranslatable            `xml:"ImportantInfo"`
	SupportedDevices       []DellDevice                `xml:"SupportedDevices>Device"`
	RevisionHistory        DellTranslatable            `xml:"RevisionHistory"`
	SupportedSystems       []DellBrand                 `xml:"SupportedSystems>Brand"`
	Criticality            DellCriticality             `xml:"Criticality"`
	FMPWrapperInformations []DellFMPWrapperInformation `xml:"FMPWrappers>FMPWrapperInformation"`
}

// DellSoftwareBundle represents a group of related software components
type DellSoftwareBundle struct {
	BundleID        string                    `xml:"bundleID,attr"`
	BundleType      string                    `xml:"bundleType,attr"`
	DateTime        time.Time                 `xml:"dateTime,attr,omitempty"`
	Identifier      string                    `xml:"identifier,attr,omitempty"`
	Path            string                    `xml:"path,attr"`
	ReleaseID       string                    `xml:"releaseID,attr,omitempty"`
	SchemaVersion   string                    `xml:"schemaVersion,attr,omitempty"`
	Size            int64                     `xml:"size,attr,omitempty"`
	VendorVersion   string                    `xml:"vendorVersion,attr,omitempty"`
	PredecessorID   string                    `xml:"predecessorID,attr,omitempty"`
	Name            DellTranslatable          `xml:"Name"`
	ComponentType   DellTranslatableWithValue `xml:"ComponentType"`
	Description     DellTranslatable          `xml:"Description"`
	Category        DellTranslatableWithValue `xml:"Category"`
	TargetSystems   []DellBrand               `xml:"TargetSystems>Brand"`
	TargetOSes      []DellOperatingSystem     `xml:"TargetOSes>OperatingSystem"`
	RevisionHistory DellTranslatable          `xml:"RevisionHistory"`
	ImportantInfo   DellTranslatable          `xml:"ImportantInfo"`
	Packages        []DellPackage             `xml:"Contents>Package"`
}

// DellBundle represents a bundle of software components
type DellBundle struct {
	ID          string   `xml:"ID,attr"`
	Name        string   `xml:"Name,attr"`
	Description string   `xml:"Description,attr,omitempty"`
	ReleaseID   string   `xml:"ReleaseID,attr,omitempty"`
	Components  []string `xml:"Components>ComponentID"`
}

// DellCategory represents a category of components
type DellCategory struct {
	ID            string         `xml:"ID,attr"`
	Name          string         `xml:"Name,attr"`
	Description   string         `xml:"Description,attr,omitempty"`
	SubCategories []DellCategory `xml:"Category,omitempty"`
}

type DellCriticality struct {
	Value int64 `xml:"value,attr"`
	DellTranslatable
}

type DellDevice struct {
	ComponentID         string                  `xml:"componentID,attr"`
	Embedded            string                  `xml:"embedded,attr"`
	PCIInfo             []DellPCIInfo           `xml:"PCIInfo"`
	Images              []DellImage             `xml:"PayloadConfiguration>Image"`
	RollbackInformation DellRollbackInformation `xml:"RollbackInformation"`
	DellTranslatable
}

// DellPCIInfo is the PCI identity a DUP device is matched on. fwupd builds the
// same instance ID from the PCI parent, so the GUIDs line up.
type DellPCIInfo struct {
	DeviceID    string `xml:"deviceID,attr" yaml:"device_id"`
	VendorID    string `xml:"vendorID,attr" yaml:"vendor_id"`
	SubDeviceID string `xml:"subDeviceID,attr" yaml:"sub_device_id"`
	SubVendorID string `xml:"subVendorID,attr" yaml:"sub_vendor_id"`
}

// DellDUPPackage is the package.xml a DUP carries. It is rooted at
// <SoftwareComponent>, not <Manifest>, so DellCatalog cannot hold it.
type DellDUPPackage struct {
	XMLName          xml.Name                  `xml:"SoftwareComponent"`
	PackageType      string                    `xml:"packageType,attr"`
	VendorVersion    string                    `xml:"vendorVersion,attr"`
	ReleaseID        string                    `xml:"releaseID,attr"`
	DateTime         time.Time                 `xml:"dateTime,attr"`
	RebootRequired   bool                      `xml:"rebootRequired,attr"`
	Criticality      DellCriticality           `xml:"Criticality"`
	Name             DellTranslatable          `xml:"Name"`
	Description      DellTranslatable          `xml:"Description"`
	LUCategory       DellTranslatableWithValue `xml:"LUCategory"`
	SupportedDevices []DellDevice              `xml:"SupportedDevices>Device"`
	SupportedSystems []DellBrand               `xml:"SupportedSystems>Brand"`
	ImportantInfo    DellImportantInfo         `xml:"ImportantInfo"`
}

// DellImportantInfo is the DUP's ImportantInfo, which carries a URL the
// catalog's DellTranslatable does not.
type DellImportantInfo struct {
	URL     string                  `xml:"URL,attr"`
	Display []DellTranslatableEntry `xml:"Display"`
}

type DellRollbackInformation struct {
	FMPIdentifier          string `xml:"fmpIdentifier,attr"`
	FMPWrapperIdentifier   string `xml:"fmpWrapperIdentifier,attr"`
	FMPWrapperVersion      string `xml:"fmpWrapperVersion,attr"`
	ImpactsTPMmeasurements bool   `xml:"impactsTPMmeasurements,attr"`
	RollbackIdentifier     string `xml:"rollbackIdentifier,attr"`
	RollbackTimeout        string `xml:"rollbackTimeout,attr"`
	RollbackVolume         string `xml:"rollbackVolume,attr"`
}

type DellImage struct {
	Filename            string                       `xml:"filename,attr"`
	ID                  string                       `xml:"id,attr"`
	Skip                bool                         `xml:"skip,attr"`
	Type                string                       `xml:"type,attr"`
	Version             string                       `xml:"version,attr"`
	ProtocolInformation DellImageProtocolInformation `xml:"ProtocolInformation"`
}
type DellImageProtocolInformation struct {
	ProtocolType string `xml:"protocolType,attr"`
}

type DellBrand struct {
	Key    int64  `xml:"key,attr"`
	Prefix string `xml:"prefix,attr"`
	DellTranslatable
	Models []DellModel `xml:"Model"`
}

type DellModel struct {
	SystemID     string `xml:"systemID,attr"`
	SystemIDType string `xml:"systemIDType,attr"`
	DellTranslatable
}

type DellOperatingSystem struct {
	OSCode   string `xml:"osCode,attr"`
	OSVendor string `xml:"osVendor,attr"`
	DellTranslatable
}

type DellPackage struct {
	Path string `xml:"path,attr"`
}

type DellFMPWrapperInformation struct {
	DigitalSignature bool                    `xml:"digitalSignature,attr"`
	DriverFileName   string                  `xml:"driverFileName,attr"`
	FilePathName     string                  `xml:"filePathName,attr"`
	Identifier       string                  `xml:"identifier,attr"`
	Name             string                  `xml:"name,attr"`
	Inventory        DellFMPWrapperInventory `xml:"Inventory"`
	Update           DellFMPWrapperUpdate    `xml:"Update"`
}

type DellFMPWrapperInventory struct {
	Source    string `xml:"source,attr"`
	Supported bool   `xml:"supported,attr"`
}

type DellFMPWrapperUpdate struct {
	Rollback  bool `xml:"rollback,attr"`
	Supported bool `xml:"supported,attr"`
}

type DellTranslatableWithValue struct {
	Value string `xml:"value,attr"`
	DellTranslatable
}

type DellTranslatable struct {
	Display []DellTranslatableEntry `xml:"Display"`
}

type DellTranslatableEntry struct {
	Lang  string `xml:"lang,attr"`
	Value string `xml:",chardata"`
}
