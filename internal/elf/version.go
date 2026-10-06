package elf

import (
	"debug/elf"
	"fmt"
)

type VersionReq struct {
	Name string
	Weak bool
}

type Verneed struct {
	File     string
	Versions []VersionReq
}

func ParseVerneed(path string) ([]Verneed, error) {
	f, err := elf.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	sec := f.Section(".gnu.version_r")
	strSec := f.Section(".dynstr")
	if sec == nil || strSec == nil {
		return nil, nil
	}

	data, err := sec.Data()
	if err != nil {
		return nil, err
	}
	strData, err := strSec.Data()
	if err != nil {
		return nil, err
	}

	order := f.ByteOrder
	var out []Verneed
	off := 0
	for off+16 <= len(data) {
		vnVersion := order.Uint16(data[off:])
		vnCnt := order.Uint16(data[off+2:])
		vnFile := order.Uint32(data[off+4:])
		vnAux := order.Uint32(data[off+8:])
		vnNext := order.Uint32(data[off+12:])
		if vnVersion != 1 {
			return nil, fmt.Errorf("unsupported verneed version %d", vnVersion)
		}

		v := Verneed{File: cstr(strData, vnFile)}
		auxOff := off + int(vnAux)
		for i := 0; i < int(vnCnt) && auxOff+16 <= len(data); i++ {
			flags := order.Uint16(data[auxOff+4:])
			name := order.Uint32(data[auxOff+8:])
			next := order.Uint32(data[auxOff+12:])
			v.Versions = append(v.Versions, VersionReq{
				Name: cstr(strData, name),
				Weak: flags&1 != 0,
			})
			if next == 0 {
				break
			}
			auxOff += int(next)
		}
		out = append(out, v)

		if vnNext == 0 {
			break
		}
		off += int(vnNext)
	}
	return out, nil
}

func ParseVerdef(path string) (map[string]bool, error) {
	f, err := elf.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	sec := f.Section(".gnu.version_d")
	strSec := f.Section(".dynstr")
	if sec == nil || strSec == nil {
		return map[string]bool{}, nil
	}

	data, err := sec.Data()
	if err != nil {
		return nil, err
	}
	strData, err := strSec.Data()
	if err != nil {
		return nil, err
	}

	order := f.ByteOrder
	out := map[string]bool{}
	off := 0
	for off+20 <= len(data) {
		vdVersion := order.Uint16(data[off:])
		vdCnt := order.Uint16(data[off+6:])
		vdAux := order.Uint32(data[off+12:])
		vdNext := order.Uint32(data[off+16:])
		if vdVersion != 1 {
			return nil, fmt.Errorf("unsupported verdef version %d", vdVersion)
		}

		auxOff := off + int(vdAux)
		for i := 0; i < int(vdCnt) && auxOff+8 <= len(data); i++ {
			name := order.Uint32(data[auxOff:])
			next := order.Uint32(data[auxOff+4:])
			out[cstr(strData, name)] = true
			if next == 0 {
				break
			}
			auxOff += int(next)
		}

		if vdNext == 0 {
			break
		}
		off += int(vdNext)
	}
	return out, nil
}

func cstr(data []byte, off uint32) string {
	if int(off) >= len(data) {
		return ""
	}
	end := int(off)
	for end < len(data) && data[end] != 0 {
		end++
	}
	return string(data[off:end])
}
