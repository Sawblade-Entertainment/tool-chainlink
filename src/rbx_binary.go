package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"math/bits"
	"sort"
	"strings"
	"sync"

	"github.com/klauspost/compress/zstd"
)

// Binary model and place files (.rbxm / .rbxl). The format is documented at
// https://github.com/rojo-rbx/rbx-dom/blob/master/docs/binary.md.

var binarySignature = []byte{0x89, 0xff, 0x0d, 0x0a, 0x1a, 0x0a}

// Property type IDs.
const (
	typeString               = 0x01
	typeBool                 = 0x02
	typeInt32                = 0x03
	typeFloat32              = 0x04
	typeFloat64              = 0x05
	typeUDim                 = 0x06
	typeUDim2                = 0x07
	typeRay                  = 0x08
	typeFaces                = 0x09
	typeAxes                 = 0x0A
	typeBrickColor           = 0x0B
	typeColor3               = 0x0C
	typeVector2              = 0x0D
	typeVector3              = 0x0E
	typeCFrame               = 0x10
	typeEnum                 = 0x12
	typeRef                  = 0x13
	typeVector3int16         = 0x14
	typeNumberSequence       = 0x15
	typeColorSequence        = 0x16
	typeNumberRange          = 0x17
	typeRect                 = 0x18
	typePhysicalProperties   = 0x19
	typeColor3uint8          = 0x1A
	typeInt64                = 0x1B
	typeSharedString         = 0x1C
	typeBytecode             = 0x1D
	typeOptionalCFrame       = 0x1E
	typeUniqueId             = 0x1F
	typeFont                 = 0x20
	typeSecurityCapabilities = 0x21
	typeContent              = 0x22
)

// maxChunkSize bounds the memory a single (possibly hostile) chunk header can request.
const maxChunkSize = 1 << 30

var zstdMagic = []byte{0x28, 0xb5, 0x2f, 0xfd}

var (
	zstdOnce sync.Once
	zstdDec  *zstd.Decoder
	zstdEnc  *zstd.Encoder
)

func zstdCodec() (*zstd.Decoder, *zstd.Encoder) {
	zstdOnce.Do(func() {
		zstdDec, _ = zstd.NewReader(nil, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(maxChunkSize))
		zstdEnc, _ = zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1))
	})
	return zstdDec, zstdEnc
}

// --- Encoding helpers ---

func transform32(x int32) uint32   { return uint32(x<<1) ^ uint32(x>>31) }
func untransform32(u uint32) int32 { return int32(u>>1) ^ -int32(u&1) }
func transform64(x int64) uint64   { return uint64(x<<1) ^ uint64(x>>63) }
func untransform64(u uint64) int64 { return int64(u>>1) ^ -int64(u&1) }

// Roblox floats move the sign bit to the bottom.
func robloxFloat(f float32) uint32 { return bits.RotateLeft32(math.Float32bits(f), 1) }
func ieeeFloat(u uint32) float32   { return math.Float32frombits(bits.RotateLeft32(u, -1)) }

// basicRotation returns the rotation matrix for a CFrame rotation ID: ID-1 is
// 6*right + up, where 0-5 name the +X, +Y, +Z, -X, -Y, -Z axes.
func basicRotation(id byte) ([9]float32, bool) {
	if id < 2 || id > 0x24 {
		return [9]float32{}, false
	}
	r, u := (id-1)/6, (id-1)%6
	if r%3 == u%3 {
		return [9]float32{}, false
	}
	axis := func(n byte) Vector3 {
		var v [3]float32
		v[n%3] = 1
		if n >= 3 {
			v[n%3] = -1
		}
		return Vector3{v[0], v[1], v[2]}
	}
	right, up := axis(r), axis(u)
	back := Vector3{
		right.Y*up.Z - right.Z*up.Y,
		right.Z*up.X - right.X*up.Z,
		right.X*up.Y - right.Y*up.X,
	}
	return [9]float32{right.X, up.X, back.X, right.Y, up.Y, back.Y, right.Z, up.Z, back.Z}, true
}

// --- Decoding ---

var errTruncated = errors.New("unexpected end of data")

type binReader struct {
	b   []byte
	err error
}

func (r *binReader) fail(err error) {
	if r.err == nil {
		r.err = err
	}
}

func (r *binReader) bytes(n int) []byte {
	if r.err != nil {
		return nil
	}
	if n < 0 || n > len(r.b) {
		r.fail(errTruncated)
		return nil
	}
	out := r.b[:n:n]
	r.b = r.b[n:]
	return out
}

func (r *binReader) u8() byte {
	if b := r.bytes(1); b != nil {
		return b[0]
	}
	return 0
}

func (r *binReader) u16() uint16 {
	if b := r.bytes(2); b != nil {
		return binary.LittleEndian.Uint16(b)
	}
	return 0
}

func (r *binReader) u32() uint32 {
	if b := r.bytes(4); b != nil {
		return binary.LittleEndian.Uint32(b)
	}
	return 0
}

func (r *binReader) f32() float32 { return math.Float32frombits(r.u32()) }

// count reads an element count, rejecting counts larger than the data left.
func (r *binReader) count() int {
	n := r.u32()
	if int64(n) > int64(len(r.b)) {
		r.fail(errTruncated)
		return 0
	}
	return int(n)
}

func (r *binReader) str() string { return string(r.bytes(r.count())) }

// interleaved reads n values of size bytes stored byte-plane by byte-plane
// and returns them back in order.
func (r *binReader) interleaved(n, size int) []byte {
	in := r.bytes(n * size)
	if in == nil {
		return make([]byte, n*size)
	}
	out := make([]byte, n*size)
	for i := 0; i < n; i++ {
		for j := 0; j < size; j++ {
			out[i*size+j] = in[j*n+i]
		}
	}
	return out
}

func (r *binReader) u32s(n int) []uint32 {
	b := r.interleaved(n, 4)
	out := make([]uint32, n)
	for i := range out {
		out[i] = binary.BigEndian.Uint32(b[i*4:])
	}
	return out
}

func (r *binReader) i32s(n int) []int32 {
	out := make([]int32, n)
	for i, u := range r.u32s(n) {
		out[i] = untransform32(u)
	}
	return out
}

func (r *binReader) f32s(n int) []float32 {
	out := make([]float32, n)
	for i, u := range r.u32s(n) {
		out[i] = ieeeFloat(u)
	}
	return out
}

func (r *binReader) i64s(n int) []int64 {
	b := r.interleaved(n, 8)
	out := make([]int64, n)
	for i := range out {
		out[i] = untransform64(binary.BigEndian.Uint64(b[i*8:]))
	}
	return out
}

// refs reads a referent array: transformed int32 deltas.
func (r *binReader) refs(n int) []int32 {
	out := r.i32s(n)
	for i := 1; i < n; i++ {
		out[i] += out[i-1]
	}
	return out
}

func (r *binReader) cframes(n int) []CFrame {
	out := make([]CFrame, n)
	for i := range out {
		id := r.u8()
		if id == 0 {
			for j := range out[i].Rotation {
				out[i].Rotation[j] = r.f32()
			}
		} else if rot, ok := basicRotation(id); ok {
			out[i].Rotation = rot
		} else {
			r.fail(fmt.Errorf("invalid CFrame rotation ID %#x", id))
		}
	}
	xs, ys, zs := r.f32s(n), r.f32s(n), r.f32s(n)
	for i := range out {
		out[i].Position = Vector3{xs[i], ys[i], zs[i]}
	}
	return out
}

type binClass struct {
	name  string
	insts []*Instance
}

type binDecoder struct {
	insts   map[int32]*Instance
	order   []int32 // referents in INST order
	classes map[uint32]*binClass
	shared  []string
	parents map[int32]int32
	prnt    []int32 // children in PRNT order
	warned  map[string]bool
}

func decodeBinary(data []byte) (*rbxFile, error) {
	if len(data) < 32 || !bytes.HasPrefix(data, binaryMagic) || !bytes.Equal(data[8:14], binarySignature) {
		return nil, errors.New("not a binary Roblox file")
	}
	if v := binary.LittleEndian.Uint16(data[14:16]); v != 0 {
		return nil, fmt.Errorf("unsupported binary format version %d", v)
	}
	d := &binDecoder{
		insts:   map[int32]*Instance{},
		classes: map[uint32]*binClass{},
		parents: map[int32]int32{},
		warned:  map[string]bool{},
	}
	f := &rbxFile{Meta: map[string]string{}}
	r := &binReader{b: data[32:]}
	for {
		hdr := r.bytes(16)
		if r.err != nil {
			return nil, errors.New("missing END chunk")
		}
		name := string(bytes.TrimRight(hdr[0:4], "\x00"))
		clen := binary.LittleEndian.Uint32(hdr[4:8])
		ulen := binary.LittleEndian.Uint32(hdr[8:12])
		if ulen > maxChunkSize {
			return nil, fmt.Errorf("%s chunk too large (%d bytes)", name, ulen)
		}
		var body []byte
		if clen == 0 {
			body = r.bytes(int(ulen))
		} else {
			raw := r.bytes(int(clen))
			if r.err == nil {
				var err error
				if body, err = decompressChunk(raw, int(ulen)); err != nil {
					return nil, fmt.Errorf("%s chunk: %w", name, err)
				}
			}
		}
		if r.err != nil {
			return nil, fmt.Errorf("%s chunk: %w", name, r.err)
		}

		c := &binReader{b: body}
		switch name {
		case "META":
			for n := c.count(); n > 0 && c.err == nil; n-- {
				k := c.str()
				f.Meta[k] = c.str()
			}
		case "SSTR":
			c.u32() // version
			for n := c.count(); n > 0 && c.err == nil; n-- {
				c.bytes(16) // MD5, unused
				d.shared = append(d.shared, c.str())
			}
		case "INST":
			d.inst(c)
		case "PROP":
			d.prop(c)
		case "PRNT":
			c.u8() // version
			n := c.count()
			children, parents := c.refs(n), c.refs(n)
			for i, child := range children {
				if _, ok := d.insts[child]; ok {
					d.parents[child] = parents[i]
					d.prnt = append(d.prnt, child)
				}
			}
		case "END":
			return d.finish(f), nil
		}
		if c.err != nil {
			return nil, fmt.Errorf("%s chunk: %w", name, c.err)
		}
	}
}

func decompressChunk(raw []byte, size int) ([]byte, error) {
	if bytes.HasPrefix(raw, zstdMagic) {
		dec, _ := zstdCodec()
		out, err := dec.DecodeAll(raw, make([]byte, 0, size))
		if err != nil {
			return nil, err
		}
		if len(out) != size {
			return nil, fmt.Errorf("decompressed to %d bytes, want %d", len(out), size)
		}
		return out, nil
	}
	return lz4Decompress(raw, size)
}

// lz4Decompress decodes a raw LZ4 block (no frame) of known decompressed size.
func lz4Decompress(src []byte, size int) ([]byte, error) {
	errCorrupt := errors.New("corrupt LZ4 data")
	if size > len(src)*255+16 {
		return nil, errCorrupt
	}
	dst := make([]byte, 0, size)
	for i := 0; i < len(src); {
		token := src[i]
		i++
		lit := int(token >> 4)
		if lit == 15 {
			for {
				if i >= len(src) {
					return nil, errCorrupt
				}
				b := src[i]
				i++
				lit += int(b)
				if b != 255 {
					break
				}
			}
		}
		if lit > len(src)-i || lit > size-len(dst) {
			return nil, errCorrupt
		}
		dst = append(dst, src[i:i+lit]...)
		i += lit
		if i == len(src) {
			break // the last sequence has no match
		}
		if i+2 > len(src) {
			return nil, errCorrupt
		}
		off := int(src[i]) | int(src[i+1])<<8
		i += 2
		ml := int(token & 15)
		if ml == 15 {
			for {
				if i >= len(src) {
					return nil, errCorrupt
				}
				b := src[i]
				i++
				ml += int(b)
				if b != 255 {
					break
				}
			}
		}
		ml += 4
		if off == 0 || off > len(dst) || ml > size-len(dst) {
			return nil, errCorrupt
		}
		start := len(dst) - off
		for k := 0; k < ml; k++ { // byte by byte: the match may overlap its output
			dst = append(dst, dst[start+k])
		}
	}
	if len(dst) != size {
		return nil, errCorrupt
	}
	return dst, nil
}

func (d *binDecoder) inst(c *binReader) {
	id := c.u32()
	class := c.str()
	service := c.u8() == 1
	n := c.count()
	refs := c.refs(n)
	if service {
		c.bytes(n)
	}
	if c.err != nil {
		return
	}
	cls := &binClass{name: class}
	for _, ref := range refs {
		if _, dup := d.insts[ref]; dup {
			c.fail(fmt.Errorf("duplicate referent %d", ref))
			return
		}
		inst := &Instance{Class: class}
		d.insts[ref] = inst
		d.order = append(d.order, ref)
		cls.insts = append(cls.insts, inst)
	}
	d.classes[id] = cls
}

func (d *binDecoder) prop(c *binReader) {
	cls := d.classes[c.u32()]
	name := c.str()
	typ := c.u8()
	if c.err != nil {
		return
	}
	if cls == nil {
		c.fail(fmt.Errorf("property %s for unknown class", name))
		return
	}
	values := d.values(c, typ, len(cls.insts), cls.name, name)
	if c.err != nil || values == nil {
		return
	}
	for i, inst := range cls.insts {
		if name == "Name" {
			if s, ok := stringValue(values[i]); ok {
				inst.Name = s
				continue
			}
		}
		inst.Props = append(inst.Props, Property{name, values[i]})
	}
}

// stringKind picks the string type of a binary String property, which the
// format itself does not record.
func stringKind(class, prop, s string) Value {
	hint := propHint(class, prop)
	switch hint {
	case "ContentId":
		return ContentId(s)
	case "ProtectedString":
		return ProtectedString(s)
	case "BinaryString":
		return BinaryString(s)
	}
	switch {
	case !xmlSafe(s):
		return BinaryString(s)
	case hint == "" && isAssetURL(s):
		return ContentId(s) // a property hidden from reflection
	}
	return String(s)
}

func isAssetURL(s string) bool {
	for _, scheme := range []string{"rbxassetid://", "rbxasset://", "rbxthumb://", "rbxhttp://", "http://", "https://"} {
		if len(s) > len(scheme) && strings.EqualFold(s[:len(scheme)], scheme) {
			return true
		}
	}
	return false
}

// values decodes the n values of one PROP chunk. It returns nil for types
// that are skipped.
func (d *binDecoder) values(c *binReader, typ byte, n int, class, prop string) []Value {
	out := make([]Value, n)
	switch typ {
	case typeString:
		for i := range out {
			out[i] = stringKind(class, prop, c.str())
		}
	case typeBool:
		for i, b := range c.bytes(n) {
			out[i] = Bool(b != 0)
		}
	case typeInt32:
		for i, v := range c.i32s(n) {
			out[i] = Int32(v)
		}
	case typeFloat32:
		for i, v := range c.f32s(n) {
			out[i] = Float32(v)
		}
	case typeFloat64:
		for i := range out {
			if b := c.bytes(8); b != nil {
				out[i] = Float64(math.Float64frombits(binary.LittleEndian.Uint64(b)))
			}
		}
	case typeUDim:
		scales, offsets := c.f32s(n), c.i32s(n)
		for i := range out {
			out[i] = UDim{scales[i], offsets[i]}
		}
	case typeUDim2:
		xs, ys, xo, yo := c.f32s(n), c.f32s(n), c.i32s(n), c.i32s(n)
		for i := range out {
			out[i] = UDim2{UDim{xs[i], xo[i]}, UDim{ys[i], yo[i]}}
		}
	case typeRay:
		for i := range out {
			out[i] = Ray{Vector3{c.f32(), c.f32(), c.f32()}, Vector3{c.f32(), c.f32(), c.f32()}}
		}
	case typeFaces:
		for i, b := range c.bytes(n) {
			out[i] = Faces(b)
		}
	case typeAxes:
		for i, b := range c.bytes(n) {
			out[i] = Axes(b)
		}
	case typeBrickColor:
		for i, v := range c.u32s(n) {
			out[i] = BrickColor(v)
		}
	case typeColor3:
		rs, gs, bs := c.f32s(n), c.f32s(n), c.f32s(n)
		for i := range out {
			out[i] = Color3{rs[i], gs[i], bs[i]}
		}
	case typeVector2:
		xs, ys := c.f32s(n), c.f32s(n)
		for i := range out {
			out[i] = Vector2{xs[i], ys[i]}
		}
	case typeVector3:
		xs, ys, zs := c.f32s(n), c.f32s(n), c.f32s(n)
		for i := range out {
			out[i] = Vector3{xs[i], ys[i], zs[i]}
		}
	case typeCFrame:
		for i, cf := range c.cframes(n) {
			out[i] = cf
		}
	case typeEnum:
		for i, v := range c.u32s(n) {
			out[i] = Enum(v)
		}
	case typeRef:
		for i, ref := range c.refs(n) {
			out[i] = Ref{d.insts[ref]} // -1 and unknown referents are empty
		}
	case typeVector3int16:
		for i := range out {
			out[i] = Vector3int16{int16(c.u16()), int16(c.u16()), int16(c.u16())}
		}
	case typeNumberSequence:
		for i := range out {
			seq := make(NumberSequence, c.count())
			for k := range seq {
				seq[k] = NumberKeypoint{c.f32(), c.f32(), c.f32()}
			}
			out[i] = seq
		}
	case typeColorSequence:
		for i := range out {
			seq := make(ColorSequence, c.count())
			for k := range seq {
				seq[k] = ColorKeypoint{Time: c.f32(), Color: Color3{c.f32(), c.f32(), c.f32()}, Envelope: c.f32()}
			}
			out[i] = seq
		}
	case typeNumberRange:
		for i := range out {
			out[i] = NumberRange{c.f32(), c.f32()}
		}
	case typeRect:
		x0, y0, x1, y1 := c.f32s(n), c.f32s(n), c.f32s(n), c.f32s(n)
		for i := range out {
			out[i] = Rect{Vector2{x0[i], y0[i]}, Vector2{x1[i], y1[i]}}
		}
	case typePhysicalProperties:
		for i := range out {
			flags := c.u8()
			pp := PhysicalProperties{}
			if flags&1 != 0 {
				pp = PhysicalProperties{true, c.f32(), c.f32(), c.f32(), c.f32(), c.f32(), 1}
				if flags&2 != 0 {
					pp.AcousticAbsorption = c.f32()
				}
			}
			out[i] = pp
		}
	case typeColor3uint8:
		rs, gs, bs := c.bytes(n), c.bytes(n), c.bytes(n)
		if c.err == nil {
			for i := range out {
				out[i] = Color3uint8{rs[i], gs[i], bs[i]}
			}
		}
	case typeInt64:
		for i, v := range c.i64s(n) {
			out[i] = Int64(v)
		}
	case typeSharedString:
		net := propHint(class, prop) == "NetAssetRef"
		for i, idx := range c.u32s(n) {
			if int(idx) >= len(d.shared) {
				c.fail(fmt.Errorf("%s.%s: shared string index %d out of range", class, prop, idx))
				return nil
			}
			if net {
				out[i] = NetAssetRef(d.shared[idx])
			} else {
				out[i] = SharedString(d.shared[idx])
			}
		}
	case typeOptionalCFrame:
		if c.u8() != typeCFrame {
			c.fail(fmt.Errorf("%s.%s: malformed OptionalCFrame", class, prop))
			return nil
		}
		cfs := c.cframes(n)
		if c.u8() != typeBool {
			c.fail(fmt.Errorf("%s.%s: malformed OptionalCFrame", class, prop))
			return nil
		}
		for i, b := range c.bytes(n) {
			if b != 0 {
				out[i] = OptionalCFrame{cfs[i], true}
			} else {
				out[i] = OptionalCFrame{}
			}
		}
	case typeUniqueId:
		b := c.interleaved(n, 16)
		for i := range out {
			v := b[i*16:]
			out[i] = UniqueId{
				Index:  binary.BigEndian.Uint32(v[0:]),
				Time:   binary.BigEndian.Uint32(v[4:]),
				Random: int64(bits.RotateLeft64(binary.BigEndian.Uint64(v[8:]), -1)),
			}
		}
	case typeFont:
		for i := range out {
			out[i] = Font{Family: c.str(), Weight: c.u16(), Style: FontStyle(c.u8()), CachedFaceId: c.str()}
		}
	case typeSecurityCapabilities:
		for i, v := range c.i64s(n) {
			out[i] = SecurityCapabilities(v)
		}
	case typeContent:
		kinds := c.i32s(n)
		uris := make([]string, c.count())
		for i := range uris {
			uris[i] = c.str()
		}
		objects := c.refs(c.count())
		c.bytes(c.count() * 4) // external objects, which cannot be resolved
		if c.err != nil {
			return nil
		}
		for i, k := range kinds {
			v := Content{}
			switch ContentKind(k) {
			case ContentUri:
				if len(uris) == 0 {
					c.fail(fmt.Errorf("%s.%s: missing Content URI", class, prop))
					return nil
				}
				v = Content{Kind: ContentUri, Uri: uris[0]}
				uris = uris[1:]
			case ContentObject:
				if len(objects) == 0 {
					c.fail(fmt.Errorf("%s.%s: missing Content object", class, prop))
					return nil
				}
				if target := d.insts[objects[0]]; target != nil {
					v = Content{Kind: ContentObject, Object: target}
				}
				objects = objects[1:]
			}
			out[i] = v
		}
	case typeBytecode:
		return nil // compiled scripts, which Studio ignores on load
	default:
		if key := fmt.Sprintf("type %#x", typ); !d.warned[key] {
			d.warned[key] = true
			warnf("skipping %s.%s: unsupported binary property type %#x", class, prop, typ)
		}
		return nil
	}
	if c.err != nil {
		return nil
	}
	return out
}

// finish links parents and children and collects the roots.
func (d *binDecoder) finish(f *rbxFile) *rbxFile {
	for _, child := range d.prnt {
		if parent := d.insts[d.parents[child]]; parent != nil && parent != d.insts[child] {
			parent.Children = append(parent.Children, d.insts[child])
		} else {
			f.Roots = append(f.Roots, d.insts[child])
		}
	}
	for _, ref := range d.order {
		if _, ok := d.parents[ref]; !ok {
			f.Roots = append(f.Roots, d.insts[ref])
		}
	}
	return f
}

// --- Encoding ---

type binWriter struct{ bytes.Buffer }

func (w *binWriter) u8(b byte) { w.WriteByte(b) }

func (w *binWriter) u16(v uint16) { w.Write(binary.LittleEndian.AppendUint16(nil, v)) }

func (w *binWriter) u32(v uint32) {
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], v)
	w.Write(b[:])
}

func (w *binWriter) f32(f float32) { w.u32(math.Float32bits(f)) }

func (w *binWriter) str(s string) {
	w.u32(uint32(len(s)))
	w.WriteString(s)
}

// interleave writes n values of size bytes byte-plane by byte-plane.
func (w *binWriter) interleave(data []byte, n, size int) {
	out := make([]byte, len(data))
	for i := 0; i < n; i++ {
		for j := 0; j < size; j++ {
			out[j*n+i] = data[i*size+j]
		}
	}
	w.Write(out)
}

func (w *binWriter) u32s(vs []uint32) {
	b := make([]byte, 4*len(vs))
	for i, v := range vs {
		binary.BigEndian.PutUint32(b[i*4:], v)
	}
	w.interleave(b, len(vs), 4)
}

func (w *binWriter) i32s(vs []int32) {
	us := make([]uint32, len(vs))
	for i, v := range vs {
		us[i] = transform32(v)
	}
	w.u32s(us)
}

func (w *binWriter) f32s(vs []float32) {
	us := make([]uint32, len(vs))
	for i, v := range vs {
		us[i] = robloxFloat(v)
	}
	w.u32s(us)
}

func (w *binWriter) i64s(vs []int64) {
	b := make([]byte, 8*len(vs))
	for i, v := range vs {
		binary.BigEndian.PutUint64(b[i*8:], transform64(v))
	}
	w.interleave(b, len(vs), 8)
}

func (w *binWriter) refs(vs []int32) {
	deltas := make([]int32, len(vs))
	prev := int32(0)
	for i, v := range vs {
		deltas[i] = v - prev
		prev = v
	}
	w.i32s(deltas)
}

func (w *binWriter) cframes(cfs []CFrame) {
	xs, ys, zs := make([]float32, len(cfs)), make([]float32, len(cfs)), make([]float32, len(cfs))
	for i, cf := range cfs {
		w.u8(0)
		for _, f := range cf.Rotation {
			w.f32(f)
		}
		xs[i], ys[i], zs[i] = cf.Position.X, cf.Position.Y, cf.Position.Z
	}
	w.f32s(xs)
	w.f32s(ys)
	w.f32s(zs)
}

// binaryType returns the property type ID a value is written as.
func binaryType(v Value) (byte, bool) {
	switch v.(type) {
	case String, ProtectedString, BinaryString, ContentId:
		return typeString, true
	case Bool:
		return typeBool, true
	case Int32:
		return typeInt32, true
	case Float32:
		return typeFloat32, true
	case Float64:
		return typeFloat64, true
	case UDim:
		return typeUDim, true
	case UDim2:
		return typeUDim2, true
	case Ray:
		return typeRay, true
	case Faces:
		return typeFaces, true
	case Axes:
		return typeAxes, true
	case BrickColor:
		return typeBrickColor, true
	case Color3:
		return typeColor3, true
	case Vector2:
		return typeVector2, true
	case Vector3:
		return typeVector3, true
	case CFrame:
		return typeCFrame, true
	case Enum:
		return typeEnum, true
	case Ref:
		return typeRef, true
	case Vector3int16:
		return typeVector3int16, true
	case NumberSequence:
		return typeNumberSequence, true
	case ColorSequence:
		return typeColorSequence, true
	case NumberRange:
		return typeNumberRange, true
	case Rect:
		return typeRect, true
	case PhysicalProperties:
		return typePhysicalProperties, true
	case Color3uint8:
		return typeColor3uint8, true
	case Int64:
		return typeInt64, true
	case SharedString, NetAssetRef:
		return typeSharedString, true
	case OptionalCFrame:
		return typeOptionalCFrame, true
	case UniqueId:
		return typeUniqueId, true
	case Font:
		return typeFont, true
	case SecurityCapabilities:
		return typeSecurityCapabilities, true
	case Content:
		return typeContent, true
	}
	return 0, false
}

// binGroup is one INST chunk: instances of one class with the same property
// names and types, so each PROP chunk has a value for every instance.
type binGroup struct {
	class   string
	insts   []*Instance
	props   []string
	types   []byte
	service bool
}

type binEncoder struct {
	refs      map[*Instance]int32
	shared    map[string]uint32
	sharedVal []string
	warned    map[string]bool
}

// encodeBinary writes f as a binary model, or as a place when place is set
// (its top-level instances are then services).
func encodeBinary(f *rbxFile, place bool) []byte {
	e := &binEncoder{refs: map[*Instance]int32{}, shared: map[string]uint32{}, warned: map[string]bool{}}
	var all []*Instance
	for _, r := range f.Roots {
		walk(r, func(inst *Instance) {
			e.refs[inst] = int32(len(all))
			all = append(all, inst)
		})
	}
	roots := map[*Instance]bool{}
	for _, r := range f.Roots {
		roots[r] = true
	}

	groups := map[string]*binGroup{}
	var keys []string
	for _, inst := range all {
		names, types := e.signature(inst)
		key := inst.Class + "\x00" + fmt.Sprint(names, types)
		g := groups[key]
		if g == nil {
			g = &binGroup{class: inst.Class, props: names, types: types}
			groups[key] = g
			keys = append(keys, key)
		}
		g.insts = append(g.insts, inst)
		g.service = g.service || (place && roots[inst])
	}
	sort.SliceStable(keys, func(i, j int) bool { return groups[keys[i]].class < groups[keys[j]].class })

	var inst, prop binWriter
	for id, key := range keys {
		g := groups[key]
		e.instChunk(&inst, uint32(id), g)
		e.propChunks(&prop, uint32(id), g)
	}

	var out bytes.Buffer
	out.Write(binaryMagic)
	out.Write(binarySignature)
	out.Write([]byte{0, 0}) // version
	out.Write(binary.LittleEndian.AppendUint32(nil, uint32(len(keys))))
	out.Write(binary.LittleEndian.AppendUint32(nil, uint32(len(all))))
	out.Write(make([]byte, 8))

	if len(f.Meta) > 0 {
		var w binWriter
		w.u32(uint32(len(f.Meta)))
		for _, k := range sortedKeys(f.Meta) {
			w.str(k)
			w.str(f.Meta[k])
		}
		writeChunk(&out, "META", w.Bytes(), true)
	}
	if len(e.sharedVal) > 0 {
		var w binWriter
		w.u32(0) // version
		w.u32(uint32(len(e.sharedVal)))
		for _, s := range e.sharedVal {
			sum := md5Sum(s)
			w.Write(sum[:])
			w.str(s)
		}
		writeChunk(&out, "SSTR", w.Bytes(), true)
	}
	splitChunks(&out, "INST", inst.Bytes())
	splitChunks(&out, "PROP", prop.Bytes())

	// Children before parents, as Studio applies PRNT entries in order.
	var children, parents []int32
	var visit func(inst *Instance, parent int32)
	visit = func(inst *Instance, parent int32) {
		for _, c := range inst.Children {
			visit(c, e.refs[inst])
		}
		children = append(children, e.refs[inst])
		parents = append(parents, parent)
	}
	for _, r := range f.Roots {
		visit(r, -1)
	}
	var w binWriter
	w.u8(0) // version
	w.u32(uint32(len(children)))
	w.refs(children)
	w.refs(parents)
	writeChunk(&out, "PRNT", w.Bytes(), true)
	writeChunk(&out, "END\x00", []byte("</roblox>"), false)
	return out.Bytes()
}

// signature lists inst's property names (Name included) and their types,
// sorted by name, skipping values the binary format cannot hold.
func (e *binEncoder) signature(inst *Instance) ([]string, []byte) {
	type entry struct {
		name string
		typ  byte
	}
	entries := []entry{{"Name", typeString}}
	for _, p := range inst.Props {
		if p.Name == "Name" {
			continue
		}
		t, ok := binaryType(p.Value)
		if !ok {
			if key := fmt.Sprintf("%T", p.Value); !e.warned[key] {
				e.warned[key] = true
				warnf("skipping %s.%s: %T cannot be stored in binary files", inst.Class, p.Name, p.Value)
			}
			continue
		}
		entries = append(entries, entry{p.Name, t})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].name < entries[j].name })
	names, types := make([]string, len(entries)), make([]byte, len(entries))
	for i, en := range entries {
		names[i], types[i] = en.name, en.typ
	}
	return names, types
}

// Chunk records are buffered back to back in one writer per chunk kind;
// each record starts with its length so splitChunks can frame them.
func (e *binEncoder) instChunk(out *binWriter, id uint32, g *binGroup) {
	var w binWriter
	w.u32(id)
	w.str(g.class)
	refs := make([]int32, len(g.insts))
	for i, inst := range g.insts {
		refs[i] = e.refs[inst]
	}
	if g.service {
		w.u8(1)
	} else {
		w.u8(0)
	}
	w.u32(uint32(len(refs)))
	w.refs(refs)
	if g.service {
		w.Write(bytes.Repeat([]byte{1}, len(refs)))
	}
	out.u32(uint32(w.Len()))
	out.Write(w.Bytes())
}

func (e *binEncoder) propChunks(out *binWriter, id uint32, g *binGroup) {
	for i, name := range g.props {
		var w binWriter
		w.u32(id)
		w.str(name)
		w.u8(g.types[i])
		values := make([]Value, len(g.insts))
		for k, inst := range g.insts {
			if name == "Name" {
				values[k] = String(inst.Name)
			} else {
				values[k] = inst.prop(name)
			}
		}
		e.values(&w, g.types[i], values)
		out.u32(uint32(w.Len()))
		out.Write(w.Bytes())
	}
}

func (e *binEncoder) ref(target *Instance) int32 {
	if r, ok := e.refs[target]; ok && target != nil {
		return r
	}
	return -1
}

func (e *binEncoder) sharedIndex(s string) uint32 {
	if i, ok := e.shared[s]; ok {
		return i
	}
	i := uint32(len(e.sharedVal))
	e.shared[s] = i
	e.sharedVal = append(e.sharedVal, s)
	return i
}

// values writes the values of one PROP chunk; every value has type typ.
func (e *binEncoder) values(w *binWriter, typ byte, vs []Value) {
	n := len(vs)
	f32 := func(get func(Value) float32) []float32 {
		out := make([]float32, n)
		for i, v := range vs {
			out[i] = get(v)
		}
		return out
	}
	i32 := func(get func(Value) int32) []int32 {
		out := make([]int32, n)
		for i, v := range vs {
			out[i] = get(v)
		}
		return out
	}
	u32 := func(get func(Value) uint32) []uint32 {
		out := make([]uint32, n)
		for i, v := range vs {
			out[i] = get(v)
		}
		return out
	}
	switch typ {
	case typeString:
		for _, v := range vs {
			s, _ := stringValue(v)
			w.str(s)
		}
	case typeBool:
		for _, v := range vs {
			if v.(Bool) {
				w.u8(1)
			} else {
				w.u8(0)
			}
		}
	case typeInt32:
		w.i32s(i32(func(v Value) int32 { return int32(v.(Int32)) }))
	case typeFloat32:
		w.f32s(f32(func(v Value) float32 { return float32(v.(Float32)) }))
	case typeFloat64:
		for _, v := range vs {
			w.Write(binary.LittleEndian.AppendUint64(nil, math.Float64bits(float64(v.(Float64)))))
		}
	case typeUDim:
		w.f32s(f32(func(v Value) float32 { return v.(UDim).Scale }))
		w.i32s(i32(func(v Value) int32 { return v.(UDim).Offset }))
	case typeUDim2:
		w.f32s(f32(func(v Value) float32 { return v.(UDim2).X.Scale }))
		w.f32s(f32(func(v Value) float32 { return v.(UDim2).Y.Scale }))
		w.i32s(i32(func(v Value) int32 { return v.(UDim2).X.Offset }))
		w.i32s(i32(func(v Value) int32 { return v.(UDim2).Y.Offset }))
	case typeRay:
		for _, v := range vs {
			r := v.(Ray)
			for _, f := range []float32{r.Origin.X, r.Origin.Y, r.Origin.Z, r.Direction.X, r.Direction.Y, r.Direction.Z} {
				w.f32(f)
			}
		}
	case typeFaces:
		for _, v := range vs {
			w.u8(byte(v.(Faces)))
		}
	case typeAxes:
		for _, v := range vs {
			w.u8(byte(v.(Axes)))
		}
	case typeBrickColor:
		w.u32s(u32(func(v Value) uint32 { return uint32(v.(BrickColor)) }))
	case typeColor3:
		w.f32s(f32(func(v Value) float32 { return v.(Color3).R }))
		w.f32s(f32(func(v Value) float32 { return v.(Color3).G }))
		w.f32s(f32(func(v Value) float32 { return v.(Color3).B }))
	case typeVector2:
		w.f32s(f32(func(v Value) float32 { return v.(Vector2).X }))
		w.f32s(f32(func(v Value) float32 { return v.(Vector2).Y }))
	case typeVector3:
		w.f32s(f32(func(v Value) float32 { return v.(Vector3).X }))
		w.f32s(f32(func(v Value) float32 { return v.(Vector3).Y }))
		w.f32s(f32(func(v Value) float32 { return v.(Vector3).Z }))
	case typeCFrame:
		cfs := make([]CFrame, n)
		for i, v := range vs {
			cfs[i] = v.(CFrame)
		}
		w.cframes(cfs)
	case typeEnum:
		w.u32s(u32(func(v Value) uint32 { return uint32(v.(Enum)) }))
	case typeRef:
		w.refs(i32(func(v Value) int32 { return e.ref(v.(Ref).Target) }))
	case typeVector3int16:
		for _, v := range vs {
			p := v.(Vector3int16)
			w.u16(uint16(p.X))
			w.u16(uint16(p.Y))
			w.u16(uint16(p.Z))
		}
	case typeNumberSequence:
		for _, v := range vs {
			seq := v.(NumberSequence)
			w.u32(uint32(len(seq)))
			for _, k := range seq {
				w.f32(k.Time)
				w.f32(k.Value)
				w.f32(k.Envelope)
			}
		}
	case typeColorSequence:
		for _, v := range vs {
			seq := v.(ColorSequence)
			w.u32(uint32(len(seq)))
			for _, k := range seq {
				w.f32(k.Time)
				w.f32(k.Color.R)
				w.f32(k.Color.G)
				w.f32(k.Color.B)
				w.f32(k.Envelope)
			}
		}
	case typeNumberRange:
		for _, v := range vs {
			r := v.(NumberRange)
			w.f32(r.Min)
			w.f32(r.Max)
		}
	case typeRect:
		w.f32s(f32(func(v Value) float32 { return v.(Rect).Min.X }))
		w.f32s(f32(func(v Value) float32 { return v.(Rect).Min.Y }))
		w.f32s(f32(func(v Value) float32 { return v.(Rect).Max.X }))
		w.f32s(f32(func(v Value) float32 { return v.(Rect).Max.Y }))
	case typePhysicalProperties:
		for _, v := range vs {
			p := v.(PhysicalProperties)
			if !p.Custom {
				w.u8(0)
				continue
			}
			w.u8(3)
			for _, f := range []float32{p.Density, p.Friction, p.Elasticity, p.FrictionWeight, p.ElasticityWeight, p.AcousticAbsorption} {
				w.f32(f)
			}
		}
	case typeColor3uint8:
		for _, part := range []func(Color3uint8) byte{
			func(c Color3uint8) byte { return c.R },
			func(c Color3uint8) byte { return c.G },
			func(c Color3uint8) byte { return c.B },
		} {
			for _, v := range vs {
				w.u8(part(v.(Color3uint8)))
			}
		}
	case typeInt64:
		out := make([]int64, n)
		for i, v := range vs {
			out[i] = int64(v.(Int64))
		}
		w.i64s(out)
	case typeSharedString:
		w.u32s(u32(func(v Value) uint32 {
			s, _ := stringValue(v)
			return e.sharedIndex(s)
		}))
	case typeOptionalCFrame:
		cfs := make([]CFrame, n)
		for i, v := range vs {
			if o := v.(OptionalCFrame); o.Set {
				cfs[i] = o.Value
			} else {
				cfs[i] = CFrame{Rotation: identityRotation}
			}
		}
		w.u8(typeCFrame)
		w.cframes(cfs)
		w.u8(typeBool)
		for _, v := range vs {
			if v.(OptionalCFrame).Set {
				w.u8(1)
			} else {
				w.u8(0)
			}
		}
	case typeUniqueId:
		b := make([]byte, 16*n)
		for i, v := range vs {
			id := v.(UniqueId)
			binary.BigEndian.PutUint32(b[i*16:], id.Index)
			binary.BigEndian.PutUint32(b[i*16+4:], id.Time)
			binary.BigEndian.PutUint64(b[i*16+8:], bits.RotateLeft64(uint64(id.Random), 1))
		}
		w.interleave(b, n, 16)
	case typeFont:
		for _, v := range vs {
			font := v.(Font)
			w.str(font.Family)
			w.u16(font.Weight)
			w.u8(byte(font.Style))
			w.str(font.CachedFaceId)
		}
	case typeSecurityCapabilities:
		out := make([]int64, n)
		for i, v := range vs {
			out[i] = int64(v.(SecurityCapabilities))
		}
		w.i64s(out)
	case typeContent:
		var uris []string
		var objects []int32
		w.i32s(i32(func(v Value) int32 {
			c := v.(Content)
			switch {
			case c.Kind == ContentUri:
				uris = append(uris, c.Uri)
				return int32(ContentUri)
			case c.Kind == ContentObject && e.ref(c.Object) >= 0:
				objects = append(objects, e.ref(c.Object))
				return int32(ContentObject)
			}
			return int32(ContentNone)
		}))
		w.u32(uint32(len(uris)))
		for _, u := range uris {
			w.str(u)
		}
		w.u32(uint32(len(objects)))
		w.refs(objects)
		w.u32(0) // external objects
	}
}

// splitChunks frames each length-prefixed record in data as its own chunk.
func splitChunks(out *bytes.Buffer, name string, data []byte) {
	for len(data) > 0 {
		n := binary.LittleEndian.Uint32(data)
		writeChunk(out, name, data[4:4+n], true)
		data = data[4+n:]
	}
}

func writeChunk(out *bytes.Buffer, name string, body []byte, compress bool) {
	var hdr [16]byte
	copy(hdr[:4], name)
	binary.LittleEndian.PutUint32(hdr[8:], uint32(len(body)))
	if compress {
		_, enc := zstdCodec()
		if c := enc.EncodeAll(body, nil); len(c) < len(body) {
			binary.LittleEndian.PutUint32(hdr[4:], uint32(len(c)))
			out.Write(hdr[:])
			out.Write(c)
			return
		}
	}
	out.Write(hdr[:])
	out.Write(body)
}
