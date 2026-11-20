package dell

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestDeviceIdentitiesIncludesThePCIIdentityAnIDRACReports(t *testing.T) {
	// The drive of an iDRAC inventory that reports "DCIM:firmware:1344:51C3:1028:2278"
	// and a SoftwareId of "0".
	device := DellDevice{
		ComponentID: "112759",
		PCIInfo:     []DellPCIInfo{{VendorID: "1344", DeviceID: "51C3", SubVendorID: "1028", SubDeviceID: "2278"}},
	}

	assert.Equal(t, []string{
		`REDFISH\VENDOR_Dell&SYSTEMID_0C60&SOFTWAREID_112759`,
		`REDFISH\VENDOR_Dell&SYSTEMID_0C60&VEN_1344&DEV_51C3&SUBSYS_10282278`,
	}, deviceIdentities("0C60", device))
}

func TestDeviceIdentitiesFormatsTheIDsAsFwupdDoes(t *testing.T) {
	device := DellDevice{
		ComponentID: "1",
		PCIInfo:     []DellPCIInfo{{VendorID: "15b3", DeviceID: "101d", SubVendorID: "15b3", SubDeviceID: "0058"}},
	}

	assert.Contains(t, deviceIdentities("0C60", device),
		`REDFISH\VENDOR_Dell&SYSTEMID_0C60&VEN_15B3&DEV_101D&SUBSYS_15B30058`,
		"fwupd writes the numbers in upper case, on four digits")
}

func TestDeviceIdentitiesNeverNamesEveryDiskThroughAComponentIDOfZero(t *testing.T) {
	device := DellDevice{ComponentID: "0"}

	assert.Empty(t, deviceIdentities("0C60", device))
}

func TestDeviceIdentitiesSkipsAPCIIdentityFwupdDoesNotBuild(t *testing.T) {
	for name, info := range map[string]DellPCIInfo{
		"all zero":          {VendorID: "0000", DeviceID: "0000", SubVendorID: "0000", SubDeviceID: "0000"},
		"zero subsystem":    {VendorID: "1344", DeviceID: "51C3", SubVendorID: "0000", SubDeviceID: "0000"},
		"missing":           {},
		"not hexadecimal":   {VendorID: "zzzz", DeviceID: "51C3", SubVendorID: "1028", SubDeviceID: "2278"},
		"more than 16 bits": {VendorID: "11344", DeviceID: "51C3", SubVendorID: "1028", SubDeviceID: "2278"},
	} {
		t.Run(name, func(t *testing.T) {
			assert.Empty(t, deviceIdentities("0C60", DellDevice{PCIInfo: []DellPCIInfo{info}}))
		})
	}
}

func TestDeviceIdentitiesListsAnIdentityOnce(t *testing.T) {
	info := DellPCIInfo{VendorID: "1344", DeviceID: "51C3", SubVendorID: "1028", SubDeviceID: "2278"}

	assert.Len(t, deviceIdentities("0C60", DellDevice{PCIInfo: []DellPCIInfo{info, info}}), 1)
}
