package main

import (
	"bytes"
	"crypto/md5"
	_ "embed"
	"fmt"
	"strings"
)

// --- Roblox DOM ---
//
// Instances carry typed property values so a tree can be read from and
// written to both file formats: XML (.rbxmx/.rbxlx, rbx_xml.go) and binary
// (.rbxm/.rbxl, rbx_binary.go).

type Instance struct {
	Class    string
	Name     string
	Props    []Property // every serialized property except Name
	Children []*Instance
	FilePath string // Source file path, relative to the project root when inside it
}

type Property struct {
	Name  string
	Value Value
}

// Value is one of the property value types below.
type Value any

type (
	String          string
	ProtectedString string // script source
	BinaryString    string // raw bytes, base64 in XML
	ContentId       string // legacy asset URL property (MeshId, Image, ...)
	SharedString    string // deduplicated blob, stored once per file
	NetAssetRef     string // SharedString-backed asset reference

	Bool                 bool
	Int32                int32
	Int64                int64
	Float32              float32
	Float64              float64
	Enum                 uint32
	BrickColor           uint32
	Faces                uint8 // bits: Right, Top, Back, Left, Bottom, Front
	Axes                 uint8 // bits: X, Y, Z
	SecurityCapabilities uint64
)

type UDim struct {
	Scale  float32
	Offset int32
}

type UDim2 struct{ X, Y UDim }

type Vector2 struct{ X, Y float32 }

type Vector3 struct{ X, Y, Z float32 }

type Vector2int16 struct{ X, Y int16 }

type Vector3int16 struct{ X, Y, Z int16 }

type Color3 struct{ R, G, B float32 }

type Color3uint8 struct{ R, G, B uint8 }

type Ray struct{ Origin, Direction Vector3 }

type Rect struct{ Min, Max Vector2 }

type NumberRange struct{ Min, Max float32 }

// CFrame stores the rotation matrix row by row (R00, R01, ... R22).
type CFrame struct {
	Position Vector3
	Rotation [9]float32
}

var identityRotation = [9]float32{1, 0, 0, 0, 1, 0, 0, 0, 1}

type OptionalCFrame struct {
	Value CFrame
	Set   bool
}

type NumberKeypoint struct{ Time, Value, Envelope float32 }

type NumberSequence []NumberKeypoint

type ColorKeypoint struct {
	Time     float32
	Color    Color3
	Envelope float32
}

type ColorSequence []ColorKeypoint

type PhysicalProperties struct {
	Custom             bool
	Density            float32
	Friction           float32
	Elasticity         float32
	FrictionWeight     float32
	ElasticityWeight   float32
	AcousticAbsorption float32
}

// Ref points at another instance; a nil Target is an empty reference.
type Ref struct{ Target *Instance }

type UniqueId struct {
	Random int64
	Time   uint32
	Index  uint32
}

type FontStyle uint8

const (
	FontNormal FontStyle = iota
	FontItalic
)

type Font struct {
	Family       string
	Weight       uint16
	Style        FontStyle
	CachedFaceId string
}

type ContentKind uint8

const (
	ContentNone ContentKind = iota
	ContentUri
	ContentObject
)

// Content is the newer asset property type (ImageContent, MeshContent, ...).
type Content struct {
	Kind   ContentKind
	Uri    string
	Object *Instance
}

// prop returns the value of inst's property name, or nil.
func (inst *Instance) prop(name string) Value {
	for _, p := range inst.Props {
		if p.Name == name {
			return p.Value
		}
	}
	return nil
}

// setProp sets name on inst, replacing any existing value.
func setProp(inst *Instance, name string, v Value) {
	for i := range inst.Props {
		if inst.Props[i].Name == name {
			inst.Props[i].Value = v
			return
		}
	}
	inst.Props = append(inst.Props, Property{Name: name, Value: v})
}

// walk calls fn for inst and every descendant, parents first.
func walk(inst *Instance, fn func(*Instance)) {
	fn(inst)
	for _, c := range inst.Children {
		walk(c, fn)
	}
}

// retarget points every Ref and object Content under root that targets old at new.
func retarget(root, old, new *Instance) {
	walk(root, func(inst *Instance) {
		for i, p := range inst.Props {
			switch v := p.Value.(type) {
			case Ref:
				if v.Target == old {
					inst.Props[i].Value = Ref{new}
				}
			case Content:
				if v.Kind == ContentObject && v.Object == old {
					v.Object = new
					inst.Props[i].Value = v
				}
			}
		}
	})
}

// --- Reflection hints ---

//go:embed rbx_reflection.txt
var reflectionData string

var propTypeHints = parseHints(reflectionData)

func parseHints(data string) map[string]string {
	hints := map[string]string{}
	for _, line := range strings.Split(data, "\n") {
		if fields := strings.Fields(line); len(fields) == 2 && !strings.HasPrefix(line, "#") {
			hints[fields[0]] = fields[1]
		}
	}
	return hints
}

// propHint returns the reflected type of class.name when the file data alone
// is ambiguous about it (see rbx_reflection.txt), or "".
func propHint(class, name string) string {
	if t, ok := propTypeHints[class+"."+name]; ok {
		return t
	}
	return propTypeHints[name]
}

// --- Files ---

// rbxFile is a decoded model or place: its top-level instances and metadata.
type rbxFile struct {
	Roots []*Instance
	Meta  map[string]string
}

var binaryMagic = []byte("<roblox!")

// decodeRobloxFile reads either file format, detected from the content.
func decodeRobloxFile(data []byte) (*rbxFile, error) {
	if bytes.HasPrefix(data, binaryMagic) {
		return decodeBinary(data)
	}
	return decodeXML(data)
}

// modelRoot turns the top-level instances of a model file into the single
// instance that stands for the file, named after it. Files with several
// top-level instances are wrapped in a Folder.
func modelRoot(f *rbxFile, name string) (*Instance, error) {
	switch len(f.Roots) {
	case 0:
		return nil, fmt.Errorf("no instances found")
	case 1:
		root := f.Roots[0]
		root.Name = name
		return root, nil
	}
	return &Instance{Class: "Folder", Name: name, Children: f.Roots}, nil
}

func md5Sum(s string) [16]byte { return md5.Sum([]byte(s)) }
