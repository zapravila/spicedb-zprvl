package blocks

import (
	"fmt"
	"strings"

	"github.com/ccoveille/go-safecast"
	v1 "github.com/zapravila/authzed-go/proto/authzed/api/v1"
	yamlv3 "gopkg.in/yaml.v3"

	"github.com/zapravila/spicedb/pkg/spiceerrors"
	"github.com/zapravila/spicedb/pkg/tuple"
)

// ParsedRelationships is the parsed relationships in a validationfile.
type ParsedRelationships struct {
	// RelationshipsString is the found string of newline-separated relationships.
	RelationshipsString string

	// SourcePosition is the position of the schema in the file.
	SourcePosition spiceerrors.SourcePosition

	// Relationships are the fully parsed relationships.
	Relationships []*v1.Relationship
}

// UnmarshalYAML is a custom unmarshaller.
func (pr *ParsedRelationships) UnmarshalYAML(node *yamlv3.Node) error {
	err := node.Decode(&pr.RelationshipsString)
	if err != nil {
		return convertYamlError(err)
	}

	relationshipsString := pr.RelationshipsString
	if relationshipsString == "" {
		return nil
	}

	seenTuples := map[string]bool{}
	lines := strings.Split(relationshipsString, "\n")
	relationships := make([]*v1.Relationship, 0, len(lines))
	for index, line := range lines {
		trimmed := strings.TrimSpace(line)
		if len(trimmed) == 0 || strings.HasPrefix(trimmed, "//") {
			continue
		}

		// +1 for the key, and *2 for newlines in YAML
		errorLine, err := safecast.ToUint64(node.Line + 1 + (index * 2))
		if err != nil {
			return err
		}
		column, err := safecast.ToUint64(node.Column)
		if err != nil {
			return err
		}

		tpl := tuple.Parse(trimmed)
		if tpl == nil {
			return spiceerrors.NewErrorWithSource(
				fmt.Errorf("error parsing relationship `%s`", trimmed),
				trimmed,
				errorLine,
				column,
			)
		}

		_, ok := seenTuples[tuple.StringWithoutCaveat(tpl)]
		if ok {
			return spiceerrors.NewErrorWithSource(
				fmt.Errorf("found repeated relationship `%s`", trimmed),
				trimmed,
				errorLine,
				column,
			)
		}
		seenTuples[tuple.StringWithoutCaveat(tpl)] = true
		relationships = append(relationships, tuple.MustToRelationship(tpl))
	}

	pr.Relationships = relationships
	pr.SourcePosition = spiceerrors.SourcePosition{LineNumber: node.Line, ColumnPosition: node.Column}
	return nil
}
