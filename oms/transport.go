package oms

import "strings"

// ClientVersion represents Opera Mini client protocol generations.
type ClientVersion uint8

const (
	ClientVersion1 ClientVersion = 1
	ClientVersion2 ClientVersion = 2
	ClientVersion3 ClientVersion = 3
)

func normalizeClientVersion(v ClientVersion) ClientVersion {
	switch v {
	case ClientVersion1, ClientVersion2, ClientVersion3:
		return v
	default:
		return ClientVersion2
	}
}

func versionHeaderByte(v ClientVersion) byte {
	switch normalizeClientVersion(v) {
	case ClientVersion1:
		return 0x0d
	case ClientVersion3:
		return 0x1a
	default:
		return 0x18
	}
}

func clientVersionFromHeaderByte(b byte) ClientVersion {
	switch b {
	case 0x0d:
		return ClientVersion1
	case 0x1a:
		return ClientVersion3
	default:
		return ClientVersion2
	}
}

// ClientVersionFromGateway heuristically maps Opera Mini gateway ids to protocol versions.
// Known values: 280 => OM2, 285 => OM3.
func ClientVersionFromGateway(gateway int) ClientVersion {
	switch {
	case gateway >= 285:
		return ClientVersion3
	case gateway >= 280:
		return ClientVersion2
	case gateway > 0:
		return ClientVersion1
	default:
		return ClientVersion2
	}
}

// CompressionMethod indicates how the OMS payload is encoded.
type CompressionMethod uint8

const (
	CompressionGzip    CompressionMethod = 0x31
	CompressionDeflate CompressionMethod = 0x32
	CompressionNone    CompressionMethod = 0x33
)

func normalizeCompression(m CompressionMethod) CompressionMethod {
	switch m {
	case CompressionGzip, CompressionDeflate, CompressionNone:
		return m
	default:
		return CompressionDeflate
	}
}

func compressionHeaderByte(m CompressionMethod) byte {
	return byte(normalizeCompression(m))
}

func compressionFromHeaderByte(b byte) CompressionMethod {
	switch b {
	case byte(CompressionGzip):
		return CompressionGzip
	case byte(CompressionNone):
		return CompressionNone
	default:
		return CompressionDeflate
	}
}

// CompressionFromParam translates Opera Mini's `e=` query value to CompressionMethod.
func CompressionFromParam(param string) CompressionMethod {
	switch strings.ToLower(strings.TrimSpace(param)) {
	case "none", "raw", "0":
		return CompressionNone
	case "gzip", "gz":
		return CompressionGzip
	case "deflate", "def", "":
		return CompressionDeflate
	default:
		return CompressionDeflate
	}
}
