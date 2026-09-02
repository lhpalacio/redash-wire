package pgwire

const (
	OidBool        uint32 = 16
	OidInt8        uint32 = 20
	OidInt4        uint32 = 23
	OidText        uint32 = 25
	OidJSON        uint32 = 114
	OidFloat8      uint32 = 701
	OidDate        uint32 = 1082
	OidTimestamp   uint32 = 1114
	OidTimestampTZ uint32 = 1184
	OidJSONB       uint32 = 3802
)

func RedashTypeToPgOID(redashType string) uint32 {
	switch redashType {
	case "string":
		return OidText
	case "integer":
		return OidInt8
	case "float":
		return OidFloat8
	case "boolean":
		return OidBool
	case "datetime":
		// Naive until the values prove otherwise: BuildRowDescription promotes a
		// column whose values all carry a zone to timestamptz.
		return OidTimestamp
	case "date":
		return OidDate
	case "json", "jsonb":
		return OidJSONB
	default:
		return OidText
	}
}

func RedashTypeToPgSize(redashType string) int16 {
	switch redashType {
	case "boolean":
		return 1
	case "integer":
		return 8
	case "float":
		return 8
	case "date":
		return 4
	case "datetime":
		return 8
	default:
		return -1
	}
}
