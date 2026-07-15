// Package operamini owns Opera Mini negotiation and OMS/OBML encoding. Exact
// minor versions remain explicit even when several currently share a legacy
// wire family.
package operamini

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"

	"operetta/oms"
)

type SupportLevel uint8

const (
	Experimental SupportLevel = iota
	Verified
)

func (s SupportLevel) String() string {
	if s == Verified {
		return "verified"
	}
	return "experimental"
}

type Version struct {
	Major      int
	Minor      int
	MinorWidth int
}

func (v Version) String() string {
	width := v.MinorWidth
	if width < 1 {
		width = 1
	}
	return fmt.Sprintf("%d.%0*d", v.Major, width, v.Minor)
}

type Dialect struct {
	Version          Version
	Family           oms.ClientVersion
	HeaderByte       byte
	StylePayloadSize int
	MaxStringBytes   int
	Support          SupportLevel
}

func ParseVersion(raw string) (Version, error) {
	s := strings.TrimSpace(raw)
	lower := strings.ToLower(s)
	if idx := strings.Index(lower, "opera mini/"); idx >= 0 {
		s = s[idx+len("opera mini/"):]
	}
	s = strings.TrimLeftFunc(s, unicode.IsSpace)
	end := 0
	for end < len(s) {
		c := s[end]
		if (c >= '0' && c <= '9') || c == '.' {
			end++
			continue
		}
		break
	}
	s = strings.Trim(s[:end], ".")
	parts := strings.Split(s, ".")
	if len(parts) == 0 || parts[0] == "" {
		return Version{}, fmt.Errorf("opera mini: invalid version %q", raw)
	}
	major, err := strconv.Atoi(parts[0])
	if err != nil || major < 1 || major > 3 {
		return Version{}, fmt.Errorf("opera mini: unsupported major version %q", raw)
	}
	minorText := "0"
	if len(parts) == 2 {
		minorText = parts[1]
		if minorText == "" {
			return Version{}, fmt.Errorf("opera mini: invalid version %q", raw)
		}
	}
	if len(parts) > 2 {
		minorText = parts[1]
		for _, extra := range parts[2:] {
			if extra == "" {
				return Version{}, fmt.Errorf("opera mini: invalid version %q", raw)
			}
			if _, err := strconv.Atoi(extra); err != nil {
				return Version{}, fmt.Errorf("opera mini: invalid version %q", raw)
			}
		}
	}
	minor, err := strconv.Atoi(minorText)
	if err != nil || minor < 0 {
		return Version{}, fmt.Errorf("opera mini: invalid minor version %q", raw)
	}
	if major == 3 && minor > 2 {
		return Version{}, fmt.Errorf("opera mini: version %q is outside supported range 1.0-3.2", raw)
	}
	return Version{Major: major, Minor: minor, MinorWidth: len(minorText)}, nil
}

func DialectFor(version Version) (Dialect, error) {
	dialect := Dialect{
		Version:        version,
		MaxStringBytes: 0xffff,
		Support:        Experimental,
	}
	switch version.Major {
	case 1:
		dialect.Family = oms.ClientVersion1
		dialect.HeaderByte = 0x0d
		dialect.StylePayloadSize = 4
	case 2:
		dialect.Family = oms.ClientVersion2
		dialect.HeaderByte = 0x18
		dialect.StylePayloadSize = 4
		if version.Minor == 6 && version.MinorWidth >= 2 {
			dialect.Support = Verified
		}
	case 3:
		if version.Minor > 2 {
			return Dialect{}, fmt.Errorf("opera mini: version %s is outside supported range", version.String())
		}
		dialect.Family = oms.ClientVersion3
		dialect.HeaderByte = 0x1a
		dialect.StylePayloadSize = 6
	default:
		return Dialect{}, fmt.Errorf("opera mini: unsupported version %s", version.String())
	}
	return dialect, nil
}

func Negotiate(raw string) (Dialect, error) {
	version, err := ParseVersion(raw)
	if err != nil {
		return Dialect{}, err
	}
	return DialectFor(version)
}
