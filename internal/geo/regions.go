package geo

import "strings"

// regionCountry maps a cloud region / location string to its ISO 3166-1 alpha-2
// country — always a valid top-level electricityMaps zone (backend ZONE_CHOICES).
// It covers the common AWS / GCP / Azure regions; an unmapped region falls
// through to IP geolocation in Detect, so this need not be exhaustive — extend
// as new regions appear. Country = where the region's datacenters physically sit.
func regionCountry(region string) (string, bool) {
	cc, ok := regionToCountry[strings.ToLower(strings.TrimSpace(region))]
	return cc, ok
}

var regionToCountry = map[string]string{
	// ── AWS ──
	"us-east-1": "US", "us-east-2": "US", "us-west-1": "US", "us-west-2": "US",
	"ca-central-1": "CA", "ca-west-1": "CA",
	"eu-west-1": "IE", "eu-west-2": "GB", "eu-west-3": "FR",
	"eu-central-1": "DE", "eu-central-2": "CH",
	"eu-north-1": "SE", "eu-south-1": "IT", "eu-south-2": "ES",
	"ap-south-1": "IN", "ap-south-2": "IN",
	"ap-southeast-1": "SG", "ap-southeast-2": "AU", "ap-southeast-3": "ID", "ap-southeast-4": "AU",
	"ap-northeast-1": "JP", "ap-northeast-2": "KR", "ap-northeast-3": "JP",
	"ap-east-1":  "HK",
	"sa-east-1":  "BR",
	"me-south-1": "BH", "me-central-1": "AE",
	"af-south-1":   "ZA",
	"il-central-1": "IL",

	// ── GCP ──
	"us-central1": "US", "us-east1": "US", "us-east4": "US", "us-east5": "US",
	"us-west1": "US", "us-west2": "US", "us-west3": "US", "us-west4": "US", "us-south1": "US",
	"northamerica-northeast1": "CA", "northamerica-northeast2": "CA",
	"southamerica-east1": "BR", "southamerica-west1": "CL",
	"europe-west1": "BE", "europe-west2": "GB", "europe-west3": "DE", "europe-west4": "NL",
	"europe-west6": "CH", "europe-west8": "IT", "europe-west9": "FR", "europe-west10": "DE", "europe-west12": "IT",
	"europe-central2": "PL", "europe-north1": "FI", "europe-southwest1": "ES",
	"asia-east1": "TW", "asia-east2": "HK",
	"asia-northeast1": "JP", "asia-northeast2": "JP", "asia-northeast3": "KR",
	"asia-south1": "IN", "asia-south2": "IN",
	"asia-southeast1": "SG", "asia-southeast2": "ID",
	"australia-southeast1": "AU", "australia-southeast2": "AU",
	"me-west1": "IL", "me-central1": "QA", "me-central2": "SA",

	// ── Azure ──
	"eastus": "US", "eastus2": "US", "centralus": "US", "northcentralus": "US",
	"southcentralus": "US", "westus": "US", "westus2": "US", "westus3": "US", "westcentralus": "US",
	"canadacentral": "CA", "canadaeast": "CA",
	"brazilsouth": "BR", "brazilsoutheast": "BR",
	"northeurope": "IE", "westeurope": "NL",
	"uksouth": "GB", "ukwest": "GB",
	"francecentral": "FR", "francesouth": "FR",
	"germanywestcentral": "DE", "germanynorth": "DE",
	"switzerlandnorth": "CH", "switzerlandwest": "CH",
	"norwayeast": "NO", "norwaywest": "NO",
	"swedencentral": "SE", "polandcentral": "PL", "italynorth": "IT", "spaincentral": "ES",
	"eastasia": "HK", "southeastasia": "SG",
	"japaneast": "JP", "japanwest": "JP",
	"koreacentral": "KR", "koreasouth": "KR",
	"centralindia": "IN", "southindia": "IN", "westindia": "IN",
	"australiaeast": "AU", "australiasoutheast": "AU", "australiacentral": "AU",
	"uaenorth": "AE", "uaecentral": "AE",
	"qatarcentral":     "QA",
	"southafricanorth": "ZA",
	"israelcentral":    "IL",
}
