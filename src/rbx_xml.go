package main

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// XML model and place files (.rbxmx / .rbxlx).

// --- Escaping ---

// sanitize drops characters that are illegal in XML 1.0 and invalid UTF-8 bytes.
func sanitize(s string) string {
	if xmlSafe(s) {
		return s
	}
	var sb strings.Builder
	sb.Grow(len(s))
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		// size==1 RuneError is an invalid byte; a literal U+FFFD is kept.
		if !(r == utf8.RuneError && size == 1) && !isIllegalXML(r) {
			sb.WriteString(s[i : i+size])
		}
		i += size
	}
	return sb.String()
}

// xmlSafe reports whether s can be written as XML text unchanged.
func xmlSafe(s string) bool {
	return utf8.ValidString(s) && !strings.ContainsFunc(s, isIllegalXML)
}

func isIllegalXML(r rune) bool {
	switch {
	case r == 0x9 || r == 0xA || r == 0xD:
		return false
	case r >= 0x20 && r <= 0xD7FF, r >= 0xE000 && r <= 0xFFFD, r >= 0x10000 && r <= 0x10FFFF:
		return false
	}
	return true
}

// escText escapes s for XML character data (and attribute values: quotes are
// escaped too). Strings with nothing to escape are returned as-is.
func escText(s string) string {
	s = sanitize(s)
	if !strings.ContainsAny(s, "&<>\"'\t\n\r") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 16)
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

func escAttr(s string) string {
	return escText(s) // xml.EscapeText already escapes '"' as &#34;
}

// cdata wraps s in a CDATA section, splitting on any "]]>" so it can't terminate early.
func cdata(s string) string {
	var b bytes.Buffer
	writeCDATA(&b, s)
	return b.String()
}

func writeCDATA(w *bytes.Buffer, s string) {
	s = sanitize(s)
	w.WriteString("<![CDATA[")
	for {
		i := strings.Index(s, "]]>")
		if i < 0 {
			break
		}
		w.WriteString(s[:i])
		w.WriteString("]]]]><![CDATA[>")
		s = s[i+3:]
	}
	w.WriteString(s)
	w.WriteString("]]>")
}

const tabs = "\t\t\t\t\t\t\t\t\t\t\t\t\t\t\t\t\t\t\t\t\t\t\t\t\t\t\t\t\t\t\t\t"

func indent(depth int) string {
	if depth <= len(tabs) {
		return tabs[:depth]
	}
	return strings.Repeat("\t", depth)
}

// --- Decoding ---

// xmlNode is one property element, read whole.
type xmlNode struct {
	name     string
	attrs    []xml.Attr
	text     string
	children []*xmlNode
}

func (n *xmlNode) attr(name string) string {
	for _, a := range n.attrs {
		if a.Name.Local == name {
			return a.Value
		}
	}
	return ""
}

// child finds a child element by name, ignoring case: files disagree on
// "min" vs "Min" and similar.
func (n *xmlNode) child(name string) *xmlNode {
	for _, c := range n.children {
		if strings.EqualFold(c.name, name) {
			return c
		}
	}
	return nil
}

func (n *xmlNode) childText(name string) string {
	if c := n.child(name); c != nil {
		return c.text
	}
	return ""
}

func readXMLNode(dec *xml.Decoder, start xml.StartElement) (*xmlNode, error) {
	n := &xmlNode{name: start.Name.Local, attrs: start.Attr}
	var text []byte
	for {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			c, err := readXMLNode(dec, t)
			if err != nil {
				return nil, err
			}
			n.children = append(n.children, c)
		case xml.CharData:
			text = append(text, t...)
		case xml.EndElement:
			n.text = string(text)
			return n, nil
		}
	}
}

func startAttr(t xml.StartElement, name string) string {
	for _, a := range t.Attr {
		if a.Name.Local == name {
			return a.Value
		}
	}
	return ""
}

// pendingRef is a Ref (or object Content) resolved once every Item is read.
type pendingRef struct {
	inst      *Instance
	prop      string
	referent  string
	asContent bool
}

// pendingShared is a SharedString use; the SharedStrings table comes last.
type pendingShared struct {
	inst *Instance
	prop string
	key  string
	net  bool
}

type xmlDecoder struct {
	dec       *xml.Decoder
	referents map[string]*Instance
	shared    map[string]string
	refs      []pendingRef
	sharedUse []pendingShared
	warned    map[string]bool
}

func decodeXML(data []byte) (*rbxFile, error) {
	d := &xmlDecoder{
		dec:       xml.NewDecoder(bytes.NewReader(data)),
		referents: map[string]*Instance{},
		shared:    map[string]string{},
		warned:    map[string]bool{},
	}
	f := &rbxFile{Meta: map[string]string{}}
	found := false
	for {
		tok, err := d.dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		start, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		if start.Name.Local != "roblox" {
			return nil, fmt.Errorf("unexpected <%s>, want <roblox>", start.Name.Local)
		}
		found = true
		if err := d.document(f); err != nil {
			return nil, err
		}
	}
	if !found {
		return nil, errors.New("no <roblox> element")
	}

	for _, p := range d.refs {
		target := d.referents[p.referent] // "null" and unknown referents stay empty
		if p.asContent {
			c := Content{}
			if target != nil {
				c = Content{Kind: ContentObject, Object: target}
			}
			setProp(p.inst, p.prop, c)
		} else {
			setProp(p.inst, p.prop, Ref{target})
		}
	}
	for _, p := range d.sharedUse {
		v, ok := d.shared[p.key]
		if !ok {
			d.warn("missing shared string "+p.key, "%s.%s uses missing shared string %s", p.inst.Class, p.prop, p.key)
		}
		if p.net {
			setProp(p.inst, p.prop, NetAssetRef(v))
		} else {
			setProp(p.inst, p.prop, SharedString(v))
		}
	}
	return f, nil
}

// warn reports a problem once per key.
func (d *xmlDecoder) warn(key, format string, args ...any) {
	if !d.warned[key] {
		d.warned[key] = true
		warnf(format, args...)
	}
}

// document reads the children of <roblox>.
func (d *xmlDecoder) document(f *rbxFile) error {
	for {
		tok, err := d.dec.Token()
		if err != nil {
			return err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "Item":
				inst, err := d.item(t)
				if err != nil {
					return err
				}
				f.Roots = append(f.Roots, inst)
			case "Meta":
				n, err := readXMLNode(d.dec, t)
				if err != nil {
					return err
				}
				f.Meta[n.attr("name")] = n.text
			case "SharedStrings":
				n, err := readXMLNode(d.dec, t)
				if err != nil {
					return err
				}
				for _, s := range n.children {
					if key := s.attr("md5"); s.name == "SharedString" && key != "" {
						v, err := decodeBase64Text(s.text)
						if err != nil {
							return fmt.Errorf("shared string %s: %w", key, err)
						}
						d.shared[key] = v
					}
				}
			default:
				if err := d.dec.Skip(); err != nil {
					return err
				}
			}
		case xml.EndElement:
			return nil
		}
	}
}

func (d *xmlDecoder) item(start xml.StartElement) (*Instance, error) {
	inst := &Instance{Class: startAttr(start, "class")}
	if inst.Class == "" {
		return nil, errors.New("<Item> without a class")
	}
	if ref := startAttr(start, "referent"); ref != "" {
		d.referents[ref] = inst
	}
	for {
		tok, err := d.dec.Token()
		if err != nil {
			return nil, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "Item":
				child, err := d.item(t)
				if err != nil {
					return nil, err
				}
				inst.Children = append(inst.Children, child)
			case "Properties":
				if err := d.properties(inst); err != nil {
					return nil, err
				}
			default:
				if err := d.dec.Skip(); err != nil {
					return nil, err
				}
			}
		case xml.EndElement:
			return inst, nil
		}
	}
}

func (d *xmlDecoder) properties(inst *Instance) error {
	for {
		tok, err := d.dec.Token()
		if err != nil {
			return err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			n, err := readXMLNode(d.dec, t)
			if err != nil {
				return err
			}
			name := n.attr("name")
			if name == "" {
				continue
			}
			if name == "Name" && n.name == "string" {
				inst.Name = n.text
				continue
			}
			v, err := d.value(inst, name, n)
			if err != nil {
				return fmt.Errorf("%s.%s: %w", inst.Class, name, err)
			}
			if v != nil {
				setProp(inst, name, v)
			}
		case xml.EndElement:
			return nil
		}
	}
}

// value converts one property element. It returns nil for properties that are
// resolved later (refs, shared strings) or skipped.
func (d *xmlDecoder) value(inst *Instance, name string, n *xmlNode) (Value, error) {
	var p numParser
	var v Value
	text := strings.TrimSpace(n.text)
	switch n.name {
	case "string":
		if propHint(inst.Class, name) == "ContentId" {
			v = ContentId(n.text)
		} else {
			v = String(n.text)
		}
	case "ProtectedString":
		v = ProtectedString(n.text)
	case "BinaryString":
		s, err := decodeBase64Text(n.text)
		if err != nil {
			return nil, err
		}
		v = BinaryString(s)
	case "Content", "ContentId":
		return d.content(inst, name, n), nil
	case "bool":
		v = Bool(strings.EqualFold(text, "true") || text == "1")
	case "int":
		i := int32(p.int(text, 32))
		if propHint(inst.Class, name) == "BrickColor" {
			v = BrickColor(i)
		} else {
			v = Int32(i)
		}
	case "BrickColor":
		v = BrickColor(p.int(text, 32))
	case "int64":
		v = Int64(p.int(text, 64))
	case "float":
		v = Float32(p.f32(text))
	case "double":
		v = Float64(p.f64(text))
	case "token":
		v = Enum(p.int(text, 32))
	case "SecurityCapabilities":
		v = SecurityCapabilities(p.int(text, 64))
	case "UDim":
		v = UDim{p.f32(n.childText("S")), int32(p.int(n.childText("O"), 32))}
	case "UDim2":
		v = UDim2{
			UDim{p.f32(n.childText("XS")), int32(p.int(n.childText("XO"), 32))},
			UDim{p.f32(n.childText("YS")), int32(p.int(n.childText("YO"), 32))},
		}
	case "Ray":
		v = Ray{p.vector3(n.child("origin")), p.vector3(n.child("direction"))}
	case "Faces":
		v = Faces(p.int(n.childText("faces"), 32))
	case "Axes":
		v = Axes(p.int(n.childText("axes"), 32))
	case "Color3":
		if len(n.children) == 0 && text != "" {
			// Older files pack the color into one integer.
			c := uint32(p.int(text, 64))
			v = Color3{float32(c>>16&0xff) / 255, float32(c>>8&0xff) / 255, float32(c&0xff) / 255}
		} else {
			v = Color3{p.f32(n.childText("R")), p.f32(n.childText("G")), p.f32(n.childText("B"))}
		}
	case "Color3uint8":
		c := uint32(p.int(text, 64))
		v = Color3uint8{uint8(c >> 16), uint8(c >> 8), uint8(c)}
	case "Vector2":
		v = p.vector2(n)
	case "Vector3":
		v = p.vector3(n)
	case "Vector2int16":
		v = Vector2int16{int16(p.int(n.childText("X"), 16)), int16(p.int(n.childText("Y"), 16))}
	case "Vector3int16":
		v = Vector3int16{int16(p.int(n.childText("X"), 16)), int16(p.int(n.childText("Y"), 16)), int16(p.int(n.childText("Z"), 16))}
	case "CoordinateFrame", "CFrame":
		v = p.cframe(n)
	case "OptionalCoordinateFrame":
		o := OptionalCFrame{}
		if c := n.child("CFrame"); c != nil {
			o = OptionalCFrame{p.cframe(c), true}
		}
		v = o
	case "Rect2D", "Rect":
		v = Rect{p.vector2(n.child("min")), p.vector2(n.child("max"))}
	case "NumberRange":
		f := strings.Fields(text)
		r := NumberRange{}
		if len(f) >= 2 {
			r = NumberRange{p.f32(f[0]), p.f32(f[1])}
		}
		v = r
	case "NumberSequence":
		f := strings.Fields(text)
		seq := NumberSequence{}
		for i := 0; i+2 < len(f); i += 3 {
			seq = append(seq, NumberKeypoint{p.f32(f[i]), p.f32(f[i+1]), p.f32(f[i+2])})
		}
		v = seq
	case "ColorSequence":
		f := strings.Fields(text)
		seq := ColorSequence{}
		for i := 0; i+4 < len(f); i += 5 {
			seq = append(seq, ColorKeypoint{p.f32(f[i]), Color3{p.f32(f[i+1]), p.f32(f[i+2]), p.f32(f[i+3])}, p.f32(f[i+4])})
		}
		v = seq
	case "PhysicalProperties":
		pp := PhysicalProperties{Custom: strings.EqualFold(strings.TrimSpace(n.childText("CustomPhysics")), "true")}
		if pp.Custom {
			pp.Density = p.f32(n.childText("Density"))
			pp.Friction = p.f32(n.childText("Friction"))
			pp.Elasticity = p.f32(n.childText("Elasticity"))
			pp.FrictionWeight = p.f32(n.childText("FrictionWeight"))
			pp.ElasticityWeight = p.f32(n.childText("ElasticityWeight"))
			pp.AcousticAbsorption = 1
			if c := n.child("AcousticAbsorption"); c != nil {
				pp.AcousticAbsorption = p.f32(c.text)
			}
		}
		v = pp
	case "UniqueId":
		id, err := parseUniqueId(text)
		if err != nil {
			return nil, err
		}
		v = id
	case "Font":
		font := Font{Family: contentURL(n.child("Family")), Weight: 400, CachedFaceId: contentURL(n.child("CachedFaceId"))}
		if w := strings.TrimSpace(n.childText("Weight")); w != "" {
			font.Weight = uint16(p.int(w, 16))
		}
		if strings.EqualFold(strings.TrimSpace(n.childText("Style")), "Italic") {
			font.Style = FontItalic
		}
		v = font
	case "Ref":
		d.refs = append(d.refs, pendingRef{inst: inst, prop: name, referent: text})
		return nil, nil
	case "SharedString", "NetAssetRef":
		d.sharedUse = append(d.sharedUse, pendingShared{inst: inst, prop: name, key: text, net: n.name == "NetAssetRef"})
		return nil, nil
	default:
		d.warn("type "+n.name, "skipping %s.%s: unsupported property type %s", inst.Class, name, n.name)
		return nil, nil
	}
	if p.err != nil {
		return nil, p.err
	}
	return v, nil
}

// content reads <Content>/<ContentId>, which holds either a legacy URL
// (ContentId) or the newer Content value.
func (d *xmlDecoder) content(inst *Instance, name string, n *xmlNode) Value {
	if c := n.child("uri"); c != nil {
		return Content{Kind: ContentUri, Uri: c.text}
	}
	if c := n.child("Ref"); c != nil {
		d.refs = append(d.refs, pendingRef{inst: inst, prop: name, referent: strings.TrimSpace(c.text), asContent: true})
		return nil
	}
	if c := n.child("url"); c != nil {
		return ContentId(c.text)
	}
	if propHint(inst.Class, name) == "Content" {
		return Content{}
	}
	if len(n.children) == 0 {
		return ContentId(n.text)
	}
	return ContentId("") // <null/>, or the obsolete <binary>/<hash> forms
}

// contentURL reads a nested content element such as Font's <Family><url>…</url></Family>.
func contentURL(n *xmlNode) string {
	if n == nil {
		return ""
	}
	if u := n.child("url"); u != nil {
		return u.text
	}
	if len(n.children) == 0 {
		return n.text
	}
	return ""
}

func decodeBase64Text(s string) (string, error) {
	s = strings.Map(func(r rune) rune {
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' {
			return -1
		}
		return r
	}, s)
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		// Some writers drop the padding.
		if b2, err2 := base64.RawStdEncoding.DecodeString(strings.TrimRight(s, "=")); err2 == nil {
			return string(b2), nil
		}
		return "", fmt.Errorf("invalid base64: %w", err)
	}
	return string(b), nil
}

// parseUniqueId reads the 32 hex digits of a UniqueId: random, time, index.
func parseUniqueId(s string) (UniqueId, error) {
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != 16 {
		return UniqueId{}, fmt.Errorf("invalid UniqueId %q", s)
	}
	return UniqueId{
		Random: int64(binary.BigEndian.Uint64(b[0:8])),
		Time:   binary.BigEndian.Uint32(b[8:12]),
		Index:  binary.BigEndian.Uint32(b[12:16]),
	}, nil
}

// numParser parses numbers, keeping the first error. Empty text is zero.
type numParser struct{ err error }

func (p *numParser) fail(err error) {
	if p.err == nil {
		p.err = err
	}
}

func (p *numParser) f64(s string) float64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		p.fail(fmt.Errorf("invalid number %q", s))
	}
	return f
}

func (p *numParser) f32(s string) float32 { return float32(p.f64(s)) }

// int parses a signed integer, also accepting unsigned values that fill all
// bits (tokens and packed colors are written unsigned).
func (p *numParser) int(s string, bits int) int64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	if i, err := strconv.ParseInt(s, 10, bits); err == nil {
		return i
	}
	u, err := strconv.ParseUint(s, 10, bits)
	if err != nil {
		p.fail(fmt.Errorf("invalid integer %q", s))
	}
	return int64(u)
}

func (p *numParser) vector2(n *xmlNode) Vector2 {
	if n == nil {
		return Vector2{}
	}
	return Vector2{p.f32(n.childText("X")), p.f32(n.childText("Y"))}
}

func (p *numParser) vector3(n *xmlNode) Vector3 {
	if n == nil {
		return Vector3{}
	}
	return Vector3{p.f32(n.childText("X")), p.f32(n.childText("Y")), p.f32(n.childText("Z"))}
}

var rotationKeys = [9]string{"R00", "R01", "R02", "R10", "R11", "R12", "R20", "R21", "R22"}

func (p *numParser) cframe(n *xmlNode) CFrame {
	cf := CFrame{Position: p.vector3(n), Rotation: identityRotation}
	if n.child("R00") != nil {
		for i, k := range rotationKeys {
			cf.Rotation[i] = p.f32(n.childText(k))
		}
	}
	return cf
}

// --- Encoding ---

type xmlEncoder struct {
	w         bytes.Buffer
	referents map[*Instance]string
	shared    map[string]string // md5 key -> value
}

// encodeXML writes f as an XML model or place.
func encodeXML(f *rbxFile) []byte {
	e := &xmlEncoder{referents: map[*Instance]string{}, shared: map[string]string{}}
	hint := 512
	for _, r := range f.Roots {
		walk(r, func(inst *Instance) {
			e.referents[inst] = fmt.Sprintf("RBX%032d", len(e.referents)+1)
			hint += 160 + len(inst.Name)
			for _, p := range inst.Props {
				hint += 64
				if s, ok := stringValue(p.Value); ok {
					hint += len(s)
				}
			}
		})
	}
	e.w.Grow(hint)

	e.w.WriteString(`<roblox xmlns:xmime="http://www.w3.org/2005/05/xmlmime" xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance" xsi:noNamespaceSchemaLocation="http://www.roblox.com/roblox.xsd" version="4">` + "\n")
	for _, k := range sortedKeys(f.Meta) {
		fmt.Fprintf(&e.w, "\t<Meta name=\"%s\">%s</Meta>\n", escAttr(k), escText(f.Meta[k]))
	}
	for _, r := range f.Roots {
		e.item(r, 1)
	}
	if len(e.shared) > 0 {
		e.w.WriteString("\t<SharedStrings>\n")
		for _, k := range sortedKeys(e.shared) {
			fmt.Fprintf(&e.w, "\t\t<SharedString md5=\"%s\">%s</SharedString>\n", k, base64.StdEncoding.EncodeToString([]byte(e.shared[k])))
		}
		e.w.WriteString("\t</SharedStrings>\n")
	}
	e.w.WriteString("</roblox>\n")
	return e.w.Bytes()
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func (e *xmlEncoder) item(inst *Instance, depth int) {
	w := &e.w
	ind := indent(depth)
	w.WriteString(ind)
	w.WriteString(`<Item class="`)
	w.WriteString(escAttr(inst.Class))
	w.WriteString(`" referent="`)
	w.WriteString(e.referents[inst])
	w.WriteString("\">\n")
	w.WriteString(ind)
	w.WriteString("\t<Properties>\n")
	w.WriteString(ind)
	w.WriteString("\t\t<string name=\"Name\">")
	w.WriteString(escText(inst.Name))
	w.WriteString("</string>\n")
	for _, p := range inst.Props {
		e.property(ind+"\t\t", p)
	}
	w.WriteString(ind)
	w.WriteString("\t</Properties>\n")
	for _, c := range inst.Children {
		e.item(c, depth+1)
	}
	w.WriteString(ind)
	w.WriteString("</Item>\n")
}

func fmtF32(f float32) string {
	switch {
	case math.IsNaN(float64(f)):
		return "NAN"
	case math.IsInf(float64(f), 1):
		return "INF"
	case math.IsInf(float64(f), -1):
		return "-INF"
	}
	return strconv.FormatFloat(float64(f), 'g', -1, 32)
}

func fmtF64(f float64) string {
	switch {
	case math.IsNaN(f):
		return "NAN"
	case math.IsInf(f, 1):
		return "INF"
	case math.IsInf(f, -1):
		return "-INF"
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}

func xmlVector2(v Vector2) string {
	return "<X>" + fmtF32(v.X) + "</X><Y>" + fmtF32(v.Y) + "</Y>"
}

func xmlVector3(v Vector3) string {
	return "<X>" + fmtF32(v.X) + "</X><Y>" + fmtF32(v.Y) + "</Y><Z>" + fmtF32(v.Z) + "</Z>"
}

func xmlCFrame(cf CFrame) string {
	var b strings.Builder
	b.WriteString(xmlVector3(cf.Position))
	for i, k := range rotationKeys {
		b.WriteString("<" + k + ">" + fmtF32(cf.Rotation[i]) + "</" + k + ">")
	}
	return b.String()
}

func xmlURL(s string) string {
	if s == "" {
		return "<null></null>"
	}
	return "<url>" + escText(s) + "</url>"
}

// sharedKey registers a shared string and returns its key: the base64 MD5
// digest, which is the only key form Studio accepts.
func (e *xmlEncoder) sharedKey(s string) string {
	sum := md5Sum(s)
	key := base64.StdEncoding.EncodeToString(sum[:])
	e.shared[key] = s
	return key
}

// property writes one property element; values with no XML form are skipped.
func (e *xmlEncoder) property(ind string, p Property) {
	tag, body := "", ""
	switch v := p.Value.(type) {
	case String:
		if xmlSafe(string(v)) {
			tag, body = "string", escText(string(v))
		} else {
			tag, body = "BinaryString", base64.StdEncoding.EncodeToString([]byte(v))
		}
	case ProtectedString:
		e.w.WriteString(ind + `<ProtectedString name="` + escAttr(p.Name) + `">`)
		writeCDATA(&e.w, string(v))
		e.w.WriteString("</ProtectedString>\n")
		return
	case BinaryString:
		tag, body = "BinaryString", base64.StdEncoding.EncodeToString([]byte(v))
	case ContentId:
		tag, body = "Content", xmlURL(string(v))
	case Content:
		tag, body = "Content", "<null></null>"
		switch {
		case v.Kind == ContentUri:
			body = "<uri>" + escText(v.Uri) + "</uri>"
		case v.Kind == ContentObject && e.referents[v.Object] != "":
			body = "<Ref>" + e.referents[v.Object] + "</Ref>"
		}
	case SharedString:
		tag, body = "SharedString", e.sharedKey(string(v))
	case NetAssetRef:
		tag, body = "NetAssetRef", e.sharedKey(string(v))
	case Bool:
		tag, body = "bool", strconv.FormatBool(bool(v))
	case Int32:
		tag, body = "int", strconv.FormatInt(int64(v), 10)
	case BrickColor:
		tag, body = "int", strconv.FormatUint(uint64(v), 10)
	case Int64:
		tag, body = "int64", strconv.FormatInt(int64(v), 10)
	case Float32:
		tag, body = "float", fmtF32(float32(v))
	case Float64:
		tag, body = "double", fmtF64(float64(v))
	case Enum:
		tag, body = "token", strconv.FormatUint(uint64(v), 10)
	case SecurityCapabilities:
		tag, body = "SecurityCapabilities", strconv.FormatUint(uint64(v), 10)
	case Faces:
		tag, body = "Faces", "<faces>"+strconv.Itoa(int(v))+"</faces>"
	case Axes:
		tag, body = "Axes", "<axes>"+strconv.Itoa(int(v))+"</axes>"
	case UDim:
		tag, body = "UDim", "<S>"+fmtF32(v.Scale)+"</S><O>"+strconv.Itoa(int(v.Offset))+"</O>"
	case UDim2:
		tag = "UDim2"
		body = "<XS>" + fmtF32(v.X.Scale) + "</XS><XO>" + strconv.Itoa(int(v.X.Offset)) +
			"</XO><YS>" + fmtF32(v.Y.Scale) + "</YS><YO>" + strconv.Itoa(int(v.Y.Offset)) + "</YO>"
	case Ray:
		tag, body = "Ray", "<origin>"+xmlVector3(v.Origin)+"</origin><direction>"+xmlVector3(v.Direction)+"</direction>"
	case Color3:
		tag, body = "Color3", "<R>"+fmtF32(v.R)+"</R><G>"+fmtF32(v.G)+"</G><B>"+fmtF32(v.B)+"</B>"
	case Color3uint8:
		tag, body = "Color3uint8", strconv.FormatUint(0xff000000|uint64(v.R)<<16|uint64(v.G)<<8|uint64(v.B), 10)
	case Vector2:
		tag, body = "Vector2", xmlVector2(v)
	case Vector3:
		tag, body = "Vector3", xmlVector3(v)
	case Vector2int16:
		tag, body = "Vector2int16", fmt.Sprintf("<X>%d</X><Y>%d</Y>", v.X, v.Y)
	case Vector3int16:
		tag, body = "Vector3int16", fmt.Sprintf("<X>%d</X><Y>%d</Y><Z>%d</Z>", v.X, v.Y, v.Z)
	case CFrame:
		tag, body = "CoordinateFrame", xmlCFrame(v)
	case OptionalCFrame:
		tag = "OptionalCoordinateFrame"
		if v.Set {
			body = "<CFrame>" + xmlCFrame(v.Value) + "</CFrame>"
		}
	case Rect:
		tag, body = "Rect2D", "<min>"+xmlVector2(v.Min)+"</min><max>"+xmlVector2(v.Max)+"</max>"
	case NumberRange:
		tag, body = "NumberRange", fmtF32(v.Min)+" "+fmtF32(v.Max)+" "
	case NumberSequence:
		var b strings.Builder
		for _, k := range v {
			b.WriteString(fmtF32(k.Time) + " " + fmtF32(k.Value) + " " + fmtF32(k.Envelope) + " ")
		}
		tag, body = "NumberSequence", b.String()
	case ColorSequence:
		var b strings.Builder
		for _, k := range v {
			b.WriteString(fmtF32(k.Time) + " " + fmtF32(k.Color.R) + " " + fmtF32(k.Color.G) + " " + fmtF32(k.Color.B) + " " + fmtF32(k.Envelope) + " ")
		}
		tag, body = "ColorSequence", b.String()
	case PhysicalProperties:
		tag = "PhysicalProperties"
		if v.Custom {
			body = "<CustomPhysics>true</CustomPhysics><Density>" + fmtF32(v.Density) +
				"</Density><Friction>" + fmtF32(v.Friction) +
				"</Friction><Elasticity>" + fmtF32(v.Elasticity) +
				"</Elasticity><FrictionWeight>" + fmtF32(v.FrictionWeight) +
				"</FrictionWeight><ElasticityWeight>" + fmtF32(v.ElasticityWeight) +
				"</ElasticityWeight><AcousticAbsorption>" + fmtF32(v.AcousticAbsorption) + "</AcousticAbsorption>"
		} else {
			body = "<CustomPhysics>false</CustomPhysics>"
		}
	case Ref:
		tag, body = "Ref", "null"
		if r := e.referents[v.Target]; v.Target != nil && r != "" {
			body = r
		}
	case UniqueId:
		var b [16]byte
		binary.BigEndian.PutUint64(b[0:8], uint64(v.Random))
		binary.BigEndian.PutUint32(b[8:12], v.Time)
		binary.BigEndian.PutUint32(b[12:16], v.Index)
		tag, body = "UniqueId", hex.EncodeToString(b[:])
	case Font:
		style := "Normal"
		if v.Style == FontItalic {
			style = "Italic"
		}
		tag = "Font"
		body = "<Family>" + xmlURL(v.Family) + "</Family><Weight>" + strconv.Itoa(int(v.Weight)) + "</Weight><Style>" + style + "</Style>"
		if v.CachedFaceId != "" {
			body += "<CachedFaceId>" + xmlURL(v.CachedFaceId) + "</CachedFaceId>"
		}
	default:
		return
	}
	e.w.WriteString(ind + "<" + tag + ` name="` + escAttr(p.Name) + `">` + body + "</" + tag + ">\n")
}

// stringValue returns the text of a string-like value.
func stringValue(v Value) (string, bool) {
	switch s := v.(type) {
	case String:
		return string(s), true
	case ProtectedString:
		return string(s), true
	case BinaryString:
		return string(s), true
	case ContentId:
		return string(s), true
	case SharedString:
		return string(s), true
	case NetAssetRef:
		return string(s), true
	}
	return "", false
}
