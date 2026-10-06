package server

import (
	"bytes"
	"encoding/binary"
)

// JPEG EXIF 保留：提取原图的 Exif APP1 段（含修正后的 Orientation），
// 在渲染输出时重新插入，从而在重新编码后保留拍摄参数、GPS、时间等元数据。
//
// JPEG 段结构：FF E1 LL LL "Exif\x00\x00" + TIFF 数据
// TIFF 结构：字节序(II/MM) + 42 + IFD0 偏移 + IFD0 条目

// extractExifAPP1 scans a JPEG byte stream and returns the complete
// Exif APP1 segment (including the FF E1 length header), or nil.
func extractExifAPP1(data []byte) []byte {
	if len(data) < 4 || data[0] != 0xFF || data[1] != 0xD8 {
		return nil
	}
	i := 2
	for i+4 <= len(data) {
		if data[i] != 0xFF {
			i++
			continue
		}
		marker := data[i+1]
		if marker == 0xDA { // SOS：压缩数据开始，扫描结束
			break
		}
		// 无长度字段的标记（SOI/RSTn/EOI/TEM）
		if marker == 0xD8 || (marker >= 0xD0 && marker <= 0xD9) || marker == 0x01 {
			i += 2
			continue
		}
		if i+4 > len(data) {
			break
		}
		segLen := int(data[i+2])<<8 | int(data[i+3])
		if segLen < 2 || i+2+segLen > len(data) {
			break
		}
		if marker == 0xE1 && segLen >= 8 && bytes.Equal(data[i+4:i+10], []byte("Exif\x00\x00")) {
			seg := make([]byte, 2+segLen)
			copy(seg, data[i:i+2+segLen])
			return seg
		}
		i += 2 + segLen
	}
	return nil
}

// fixExifOrientation rewrites the Orientation tag (0x0112) of the IFD0 to 1,
// because the pipeline has already physically rotated the pixels. Keeping the
// original value would make viewers rotate the photo a second time.
func fixExifOrientation(app1 []byte) []byte {
	if len(app1) < 14 {
		return app1
	}
	tiff := app1[10:] // 跳过 FF E1 LL LL + "Exif\0\0"
	if len(tiff) < 8 {
		return app1
	}
	var bo binary.ByteOrder
	switch {
	case tiff[0] == 'I' && tiff[1] == 'I':
		bo = binary.LittleEndian
	case tiff[0] == 'M' && tiff[1] == 'M':
		bo = binary.BigEndian
	default:
		return app1
	}
	if bo.Uint16(tiff[2:4]) != 42 {
		return app1
	}
	ifd0Off := int(bo.Uint32(tiff[4:8]))
	if ifd0Off+2 > len(tiff) {
		return app1
	}
	count := int(bo.Uint16(tiff[ifd0Off : ifd0Off+2]))
	entries := ifd0Off + 2
	if entries+count*12 > len(tiff) {
		return app1
	}
	for i := 0; i < count; i++ {
		e := entries + i*12
		if bo.Uint16(tiff[e:e+2]) == 0x0112 { // Orientation
			bo.PutUint16(tiff[e+8:e+10], 1) // SHORT：按 TIFF 字节序写回 1
			return app1
		}
	}
	return app1
}

// injectAPP1 inserts the Exif APP1 segment right after the SOI (or after the
// JFIF APP0 if present), which is where image viewers expect it.
func injectAPP1(jpegData []byte, app1 []byte) []byte {
	if len(jpegData) < 4 || len(app1) == 0 {
		return jpegData
	}
	insertAt := 2
	if jpegData[2] == 0xFF && len(jpegData) > 6 && jpegData[3] == 0xE0 {
		segLen := int(jpegData[4])<<8 | int(jpegData[5])
		if segLen >= 2 && 2+2+segLen <= len(jpegData) {
			insertAt = 2 + 2 + segLen
		}
	}
	out := make([]byte, 0, len(jpegData)+len(app1))
	out = append(out, jpegData[:insertAt]...)
	out = append(out, app1...)
	out = append(out, jpegData[insertAt:]...)
	return out
}
