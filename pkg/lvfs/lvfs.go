package lvfs

import "encoding/xml"

// MetadataSchemaVersion is bumped when components are built differently, which
// discards the stored metadata and forces a full reprocessing.
// 2: component identity includes the systems a package applies to.
const MetadataSchemaVersion = 2

type Components struct {
	XMLName       xml.Name    `xml:"components"`
	SchemaVersion int         `xml:"schema_version,attr,omitempty"`
	Origin        string      `xml:"origin,attr"`
	Component     []Component `xml:"component"`
}

type Component struct {
	XMLName           xml.Name    `xml:"component"`
	Type              string      `xml:"type,attr"`
	ID                string      `xml:"id"`
	Name              string      `xml:"name"`
	NameVariantSuffix string      `xml:"name_variant_suffix,omitempty"`
	Summary           string      `xml:"summary"`
	DeveloperName     string      `xml:"developer_name,omitempty"`
	Description       Description `xml:"description"`
	Provides          []Firmware  `xml:"provides>firmware"`
	URL               URL         `xml:"url,omitempty"`
	MetadataLicense   string      `xml:"metadata_license"`
	ProjectLicense    string      `xml:"project_license"`
	Releases          []Release   `xml:"releases>release"`
	Requires          Requires    `xml:"requires,omitempty"`
	Custom            []Custom    `xml:"custom>value,omitempty"`
	Keywords          []string    `xml:"keywords>keyword,omitempty"`
	Categories        []string    `xml:"categories>category,omitempty"`
}

type Firmware struct {
	Type string `xml:"type,attr"`
	Text string `xml:",chardata"`
}

type URL struct {
	Type string `xml:"type,attr"`
	Text string `xml:",chardata"`
}

type Release struct {
	Urgency         string      `xml:"urgency,attr,omitempty"`
	Version         string      `xml:"version,attr"`
	Date            string      `xml:"date,attr"`
	InstallDuration int         `xml:"install_duration,attr"`
	Location        string      `xml:"location,omitempty"`
	Checksums       []Checksum  `xml:"checksum"`
	Description     Description `xml:"description"`
	Issues          []Issue     `xml:"issues>issue,omitempty"`
	Artifacts       []Artifact  `xml:"artifacts>artifact,omitempty"`
}

type Artifact struct {
	Type      string     `xml:"type,attr,omitempty"`
	Location  string     `xml:"location"`
	Checksums []Checksum `xml:"checksum"`
}

type Checksum struct {
	Filename string `xml:"filename,attr,omitempty"`
	Target   string `xml:"target,attr,omitempty"`
	Type     string `xml:"type,attr,omitempty"`
	Value    string `xml:",chardata"`
}

type Issue struct {
	Type string `xml:"type,attr"`
	Text string `xml:",chardata"`
}

type Requires struct {
	ID       []ID       `xml:"id"`
	Firmware []Firmware `xml:"firmware"`
}

type ID struct {
	Compare string `xml:"compare,attr"`
	Version string `xml:"version,attr"`
	Text    string `xml:",chardata"`
}

type Custom struct {
	Key   string `xml:"key,attr"`
	Value string `xml:",chardata"`
}

type Description struct {
	Value string `xml:",innerxml"`
}
