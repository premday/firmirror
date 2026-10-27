package bkc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"go.yaml.in/yaml/v3"

	"github.com/premday/firmirror/pkg/utils"
)

// fileVersion is the only layout of the file.
const fileVersion = 1

// File is the definition of the rollout rings: which version of each component
// every ring serves.
type File struct {
	Version int `yaml:"version"`
	// Rings are the rollout rings in promotion order: the first takes the newest
	// mirrored release, each other is promoted from the one before it.
	Rings []string `yaml:"rings"`
	// TagPrefix prefixes the ring name in every published tag.
	TagPrefix  string  `yaml:"tag_prefix,omitempty"`
	Components []Entry `yaml:"components"`

	// Unknown holds the unrecognized top-level fields so that parse can decide
	// whether to refuse them. It is never written.
	Unknown map[string]any `yaml:",inline"`
}

// Entry is the version each ring serves of one component.
type Entry struct {
	ID string `yaml:"id"`
	// Platform tells apart the platforms an ID covers, which are not all shipped
	// the same versions; see Platform.
	Platform string `yaml:"platform,omitempty"`
	// Name is informational.
	Name string `yaml:"name,omitempty"`
	// Hold keeps the component out of every ring, whatever Versions says, and out
	// of promotions: for one that another makes redundant, such as an older package
	// of a device that the other also updates, which no ring should serve.
	Hold     bool              `yaml:"hold,omitempty"`
	Versions map[string]string `yaml:"versions"`
}

// Key identifies the component the entry is about, as keyOf does for a mirrored one.
func (e Entry) Key() string {
	return entryKey(e.ID, e.Platform)
}

// Served returns the version a ring serves of the component: none when the entry
// names none for it, or is held.
func (e Entry) Served(ring string) string {
	if e.Hold {
		return ""
	}
	return e.Versions[ring]
}

// HoldsWhole reports whether the entry holds its ID whole, as a held one does that
// has no platform; one with a platform holds that component only.
func (e Entry) HoldsWhole() bool {
	return e.Hold && e.Platform == ""
}

// Serves reports whether some entry has a ring serve a version, so that its tag is
// published.
func (f *File) Serves(ring string) bool {
	return slices.ContainsFunc(f.Components, func(entry Entry) bool { return entry.Served(ring) != "" })
}

// HeldOnly reports whether a ring serves nothing for lack of an entry that is not
// held: every entry that names a version for it is held, and some does.
func (f *File) HeldOnly(ring string) bool {
	return !f.Serves(ring) && slices.ContainsFunc(f.Components, func(entry Entry) bool { return entry.Hold && entry.Versions[ring] != "" })
}

// New returns a file with the given rings and no component.
func New(rings []string) *File {
	return &File{Version: fileVersion, Rings: slices.Clone(rings), TagPrefix: DefaultTagPrefix}
}

// MaxFileSize caps what is read of a file, so a URL cannot fill the memory.
const MaxFileSize = 16 << 20

// ReadTimeout bounds a read of the definitions, which something always waits for.
const ReadTimeout = time.Minute

// warnedUnknown holds the unknown fields already warned of: a service ahead of this
// firmirror is read all the time, and the warning once is enough.
var warnedUnknown sync.Map

// Load reads the file from a path or a URL. An unknown top-level field is an
// error in a path, where it is most likely a typo, and a warning in a URL, whose
// service can be ahead of this firmirror (`version` gates incompatible layouts).
// Inside a component it is always an error: a mistyped `versions` would leave the
// component served by no ring, silently. Plain HTTP is refused unless allowHTTP
// says the network is trusted. The read is bounded by ReadTimeout.
func Load(ctx context.Context, source string, allowHTTP bool) (*File, error) {
	ctx, cancel := context.WithTimeout(ctx, ReadTimeout)
	defer cancel()
	data, err := utils.ReadSource(ctx, source, MaxFileSize, !allowHTTP)
	if err != nil {
		return nil, fmt.Errorf("reading the rollout rings: %w", err)
	}
	file, err := parse(data, utils.IsRemote(source))
	if err != nil {
		return nil, fmt.Errorf("reading the rollout rings from %s: %w", utils.Redacted(source), err)
	}
	return file, nil
}

// Parse reads a file strictly, as one that is edited.
func Parse(data []byte) (*File, error) {
	return parse(data, false)
}

// parse reads a file; with ignoreUnknown, unknown top-level fields are ignored
// with a warning and not an error.
func parse(data []byte, ignoreUnknown bool) (*File, error) {
	// The version gates layouts this firmirror cannot read, so it is judged before
	// the fields are.
	if err := checkNodes(data); err != nil {
		return nil, err
	}
	var file File
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&file); err != nil {
		return nil, fmt.Errorf("invalid YAML: %w", err)
	}
	// Reject what the decoder would drop silently: a second document, and an empty
	// list item such as a half-edited `-`.
	for {
		var next yaml.Node
		err := decoder.Decode(&next)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("invalid YAML: after the first document: %w", err)
		}
		// A trailing `---` is an empty document, which is fine.
		if !blankDocument(&next) {
			return nil, errors.New("invalid YAML: more than one document")
		}
	}
	if len(file.Unknown) > 0 {
		fields := slices.Sorted(maps.Keys(file.Unknown))
		if !ignoreUnknown {
			return nil, fmt.Errorf("invalid YAML: field %s not found in type bkc.File", strings.Join(fields, ", "))
		}
		level := slog.LevelDebug
		if _, warned := warnedUnknown.LoadOrStore(strings.Join(fields, ","), struct{}{}); !warned {
			level = slog.LevelWarn
		}
		slog.Log(context.Background(), level, "The rollout rings have fields this firmirror does not know, ignoring them", "fields", fields)
		file.Unknown = nil
	}
	if file.TagPrefix == "" {
		file.TagPrefix = DefaultTagPrefix
	}
	if err := file.Validate(); err != nil {
		return nil, err
	}
	return &file, nil
}

// Blank reports whether data holds only whitespace, comments and document
// markers.
func Blank(data []byte) bool {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	for {
		var document yaml.Node
		err := decoder.Decode(&document)
		if errors.Is(err, io.EOF) {
			return true
		}
		if err != nil || !blankDocument(&document) {
			return false
		}
	}
}

// checkHold refuses a hold left blank, which would read as off and serve what was
// to be held, however the blank gets to the entry: written there, through an alias,
// or through a merge key, which an explicit hold overrides. A mapping is walked
// once, which ends a merge that reaches itself and keeps a merge of merges linear.
func checkHold(entry *yaml.Node, seen map[*yaml.Node]bool) error {
	entry = resolveAlias(entry)
	if entry.Kind != yaml.MappingNode || seen[entry] {
		return nil
	}
	seen[entry] = true
	var merged []*yaml.Node
	for i := 0; i+1 < len(entry.Content); i += 2 {
		key, value := entry.Content[i], resolveAlias(entry.Content[i+1])
		switch key.Value {
		case "hold":
			if value.Kind == yaml.ScalarNode && value.Tag == "!!null" {
				return fmt.Errorf("invalid YAML: line %d: hold has no value: write true, or false", key.Line)
			}
			return nil
		case "<<":
			if value.Kind == yaml.SequenceNode {
				merged = append(merged, value.Content...)
			} else {
				merged = append(merged, value)
			}
		}
	}
	for _, one := range merged {
		if err := checkHold(one, seen); err != nil {
			return err
		}
	}
	return nil
}

func resolveAlias(node *yaml.Node) *yaml.Node {
	for node.Kind == yaml.AliasNode {
		node = node.Alias
	}
	return node
}

// blankDocument reports whether a document holds nothing, as `---` alone does.
func blankDocument(document *yaml.Node) bool {
	return document.Kind == yaml.DocumentNode && len(document.Content) == 1 &&
		document.Content[0].Kind == yaml.ScalarNode && document.Content[0].Tag == "!!null" && document.Content[0].Value == ""
}

// checkNodes fails on an empty item in the rings or components list, on a hold
// with no value, and on a version that is not an integer, which decoding into an
// int would truncate, or that this firmirror does not know.
func checkNodes(data []byte) error {
	var document yaml.Node
	if err := yaml.Unmarshal(data, &document); err != nil {
		return fmt.Errorf("invalid YAML: %w", err)
	}
	if document.Kind != yaml.DocumentNode || len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return nil
	}
	fields := document.Content[0].Content
	seen := map[*yaml.Node]bool{}
	for i := 0; i+1 < len(fields); i += 2 {
		if fields[i].Value == "version" {
			if fields[i+1].Kind == yaml.ScalarNode && fields[i+1].Tag != "!!int" {
				return fmt.Errorf("invalid YAML: line %d: the version must be an integer", fields[i+1].Line)
			}
			if version, err := strconv.Atoi(fields[i+1].Value); err == nil && version != fileVersion {
				return versionError(version)
			}
			continue
		}
		if fields[i].Value != "rings" && fields[i].Value != "components" {
			continue
		}
		for _, item := range fields[i+1].Content {
			if item.Kind == yaml.ScalarNode && item.Tag == "!!null" {
				return fmt.Errorf("invalid YAML: line %d: an empty item in %s", item.Line, fields[i].Value)
			}
			if fields[i].Value == "components" {
				if err := checkHold(item, seen); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func versionError(version int) error {
	return fmt.Errorf("version %d is not one this firmirror knows, the version is %d", version, fileVersion)
}

// Validate rejects a file that could not be published unambiguously.
func (f *File) Validate() error {
	if f.Version != fileVersion {
		return versionError(f.Version)
	}
	if !namePattern.MatchString(f.prefix()) {
		return fmt.Errorf("tag prefix %q must be lowercase alphanumeric, optionally hyphenated, and not end with a hyphen", f.prefix())
	}
	if len(f.Rings) == 0 {
		return fmt.Errorf("no ring declared")
	}
	seen := make(map[string]struct{}, len(f.Rings))
	for _, ring := range f.Rings {
		if !namePattern.MatchString(ring) {
			return fmt.Errorf("ring name %q must be lowercase alphanumeric, optionally hyphenated", ring)
		}
		if _, duplicate := seen[ring]; duplicate {
			return fmt.Errorf("ring %q is declared twice", ring)
		}
		seen[ring] = struct{}{}
	}

	keys := make(map[string]struct{}, len(f.Components))
	for _, entry := range f.Components {
		if entry.ID == "" {
			return fmt.Errorf("a component has no id")
		}
		if _, duplicate := keys[entry.Key()]; duplicate {
			return fmt.Errorf("component %q is declared twice for the same platform", entry.ID)
		}
		keys[entry.Key()] = struct{}{}
		for _, ring := range slices.Sorted(maps.Keys(entry.Versions)) {
			version := entry.Versions[ring]
			if _, ok := seen[ring]; !ok {
				return fmt.Errorf("component %q serves ring %q, which is not declared", entry.ID, ring)
			}
			if version == "" {
				return fmt.Errorf("component %q serves no version in ring %q", entry.ID, ring)
			}
		}
	}
	return nil
}

// Clone returns a copy that can be edited without changing f.
func (f *File) Clone() *File {
	clone := *f
	clone.Rings = slices.Clone(f.Rings)
	clone.Components = make([]Entry, len(f.Components))
	for i, entry := range f.Components {
		entry.Versions = maps.Clone(entry.Versions)
		clone.Components[i] = entry
	}
	return &clone
}

// Marshal encodes the file with components sorted by key and versions in ring
// order, so two promotions differ only by the lines they change.
func (f *File) Marshal() ([]byte, error) {
	components := slices.Clone(f.Components)
	slices.SortFunc(components, func(a, b Entry) int { return strings.Compare(a.Key(), b.Key()) })

	root := mapping()
	add(root, "version", scalar(fmt.Sprint(f.Version), 0))
	rings := &yaml.Node{Kind: yaml.SequenceNode, Style: yaml.FlowStyle}
	for _, ring := range f.Rings {
		rings.Content = append(rings.Content, text(ring))
	}
	add(root, "rings", rings)
	if f.prefix() != DefaultTagPrefix {
		add(root, "tag_prefix", text(f.prefix()))
	}
	list := &yaml.Node{Kind: yaml.SequenceNode}
	for _, entry := range components {
		item := mapping()
		add(item, "id", text(entry.ID))
		if entry.Platform != "" {
			add(item, "platform", text(entry.Platform))
		}
		if entry.Name != "" {
			add(item, "name", text(entry.Name))
		}
		if entry.Hold {
			add(item, "hold", scalar("true", 0))
		}
		versions := mapping()
		for _, ring := range f.Rings {
			if version := entry.Versions[ring]; version != "" {
				add(versions, ring, scalar(version, yaml.DoubleQuotedStyle))
			}
		}
		versions.Style = yaml.FlowStyle
		add(item, "versions", versions)
		list.Content = append(list.Content, item)
	}
	add(root, "components", list)

	var out bytes.Buffer
	encoder := yaml.NewEncoder(&out)
	encoder.SetIndent(2)
	if err := encoder.Encode(&yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{root}}); err != nil {
		return nil, err
	}
	if err := encoder.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func mapping() *yaml.Node {
	return &yaml.Node{Kind: yaml.MappingNode}
}

// text is a string node the encoder quotes when a plain scalar would be read as
// something else, such as a ring named null or a digest like 12e4. A value with a
// control character is always quoted, as a literal block of one may not be read back.
// So is one YAML 1.1 reads as a boolean, which linters and other tools still do.
func text(value string) *yaml.Node {
	node := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value}
	if strings.ContainsFunc(value, unicode.IsControl) || yaml11Booleans[strings.ToLower(value)] {
		node.Style = yaml.DoubleQuotedStyle
	}
	return node
}

var yaml11Booleans = map[string]bool{"y": true, "yes": true, "n": true, "no": true, "on": true, "off": true}

func scalar(value string, style yaml.Style) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Value: value, Style: style}
}

func add(node *yaml.Node, key string, value *yaml.Node) {
	node.Content = append(node.Content, text(key), value)
}

// Write replaces the file at path, all or nothing, as Marshal encodes it: comments
// in the old file are lost. A symlink is followed and the mode is kept. A file
// that Parse would not read back is not written.
func (f *File) Write(path string) error {
	data, err := f.Marshal()
	if err != nil {
		return err
	}
	if _, err := Parse(data); err != nil {
		return fmt.Errorf("the rollout rings would not be read back: %w", err)
	}
	path, err = FollowLinks(path)
	if err != nil {
		return err
	}
	mode := os.FileMode(0o644)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	}
	return utils.WriteFileAtomic(path, mode, bytes.NewReader(data))
}

// FollowLinks returns where a chain of symlinks ends, which may be a file that
// does not exist yet. A relative target is relative to the directory the link is
// really in. A chain that does not end is an error.
func FollowLinks(path string) (string, error) {
	for range 40 {
		info, err := os.Lstat(path)
		if err != nil || info.Mode()&os.ModeSymlink == 0 {
			return path, nil
		}
		target, err := os.Readlink(path)
		if err != nil {
			return "", err
		}
		if !filepath.IsAbs(target) {
			dir := filepath.Dir(path)
			if resolved, err := filepath.EvalSymlinks(dir); err == nil {
				dir = resolved
			}
			target = filepath.Join(dir, target)
		}
		path = target
	}
	return "", fmt.Errorf("%s: too many levels of symbolic links", path)
}

// Equal reports whether two files marshal the same.
func (f *File) Equal(other *File) bool {
	mine, err := f.Marshal()
	if err != nil {
		return false
	}
	theirs, err := other.Marshal()
	return err == nil && bytes.Equal(mine, theirs)
}
