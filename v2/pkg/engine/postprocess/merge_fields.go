package postprocess

import (
	"bytes"
	"log"
	"os"

	"github.com/wundergraph/graphql-go-tools/v2/pkg/engine/resolve"
)

var debugMergeFields = os.Getenv("DEBUG_MERGE_FIELDS") == "1"

type mergeFields struct {
	disable bool
}

func (m *mergeFields) Process(node resolve.Node) {
	if m.disable {
		return
	}
	m.traverseNode(node)
}

func (m *mergeFields) ProcessSubscription(node resolve.Node) {
	if m.disable {
		return
	}
	m.traverseNode(node)
}

func (m *mergeFields) traverseNode(node resolve.Node) {
	switch n := node.(type) {
	case *resolve.Object:
		if len(n.Fields) == 1 {
			m.traverseNode(n.Fields[0].Value)
			return
		}
		for i := 0; i < len(n.Fields); i++ {
			// 1. duplicate fields with multiple onTypeNames so they can be merged
			if len(n.Fields[i].OnTypeNames) >= 1 {
				additionalTypeNames := make([][]byte, len(n.Fields[i].OnTypeNames)-1)
				copy(additionalTypeNames, n.Fields[i].OnTypeNames[1:])
				n.Fields[i].OnTypeNames = [][]byte{n.Fields[i].OnTypeNames[0]}
				for j := 0; j < len(additionalTypeNames); j++ {
					additionalField := &resolve.Field{
						Name:        n.Fields[i].Name,
						Value:       n.Fields[i].Value.Copy(),
						Position:    n.Fields[i].Position,
						OnTypeNames: [][]byte{additionalTypeNames[j]},
						Info:        n.Fields[i].Info,
					}
					n.Fields = append(n.Fields[:i+1], append([]*resolve.Field{additionalField}, n.Fields[i+1:]...)...)
				}
			}
			// 2.
			// Propagate onTypeNames to all children
			// In a later stage, we merge all nested scalar fields with the same name
			// However, scalar fields can originate from different parent types, which is why we need to propagate them here
			m.propagateParentTypeNames(n.Fields[i])
		}
		// 3. merge fields without onTypeNames "over" fields with onTypeNames
		// This is possible because if a field exists without onTypeNames, it will always be resolved
		// There are 2 variants of this:
		// 3.1. if the source and target fields are scalars, the target (with onTypeNames) will be removed
		// This means that scalars with no onTypeNames will overwrite scalars with onTypeNames
		// 3.2. if the source and target fields are objects, all fields from the source will be merged into the target
		// This means that objects with no onTypeNames will be merged into objects with onTypeNames
		for i := 0; i < len(n.Fields); i++ {
			if n.Fields[i].OnTypeNames != nil {
				continue
			}
			for j := 0; j < len(n.Fields); j++ {
				if i == j {
					continue
				}
				if n.Fields[j].OnTypeNames == nil {
					continue
				}
				if bytes.Equal(n.Fields[i].Name, n.Fields[j].Name) {
					m.mergeValues(n.Fields[i], n.Fields[j])
					n.Fields = append(n.Fields[:j], n.Fields[j+1:]...)
					if i > j {
						i--
					}
					j--
				}
			}
		}
		// 4. merge sibling object fields
		for i := 0; i < len(n.Fields); i++ {
			for j := i + 1; j < len(n.Fields); j++ {
				if m.fieldsCanMerge(n.Fields[i], n.Fields[j]) {
					m.mergeValues(n.Fields[i], n.Fields[j])
					n.Fields = append(n.Fields[:j], n.Fields[j+1:]...)
					j--
				}
			}
		}
		// 5. merge sibling scalar fields
		// Once all objects have been merged, we need to merge (deduplicate) all scalar fields that are left
		for i := 0; i < len(n.Fields); i++ {
			// skip objects
			if m.nodeIsScalar(n.Fields[i].Value) {
				for j := 0; j < len(n.Fields); j++ {
					if i == j {
						continue
					}
					if bytes.Equal(n.Fields[i].Name, n.Fields[j].Name) {
						// we don't merge scalars with different onTypeNames to preserve the order of the fields
						if !m.canMergeScalars(n.Fields[i], n.Fields[j]) {
							continue
						}
						m.mergeScalars(n.Fields[i], n.Fields[j])
						n.Fields = append(n.Fields[:j], n.Fields[j+1:]...)
						if i > j {
							i--
						}
						j--
					}
				}
			}
		}
		// 6. merge sibling array fields (deduplicate arrays similar to scalars)
		for i := 0; i < len(n.Fields); i++ {
			// only arrays
			switch n.Fields[i].Value.(type) {
			case *resolve.Array:
				for j := 0; j < len(n.Fields); j++ {
					if i == j {
						continue
					}
					if !bytes.Equal(n.Fields[i].Name, n.Fields[j].Name) {
						continue
					}
					if _, ok := n.Fields[j].Value.(*resolve.Array); !ok {
						continue
					}
					if !m.canMergeArrays(n.Fields[i], n.Fields[j]) {
						continue
					}
					m.mergeArrays(n.Fields[i], n.Fields[j])
					n.Fields = append(n.Fields[:j], n.Fields[j+1:]...)
					if i > j {
						i--
					}
					j--
				}
			}
		}
		for i := 0; i < len(n.Fields); i++ {
			m.traverseNode(n.Fields[i].Value)
		}
		if debugMergeFields {
			debugDumpMergeFields(n)
		}
	case *resolve.Array:
		m.traverseNode(n.Item)
	}
}

func debugDumpMergeFields(obj *resolve.Object) {
	for _, f := range obj.Fields {
		name := string(f.Name)
		log.Printf("[merge_fields] field=%s onType=%q parentOnType=%+v path=%v",
			name,
			bytesSliceToStrings(f.OnTypeNames),
			f.ParentOnTypeNames,
			f.Value.NodePath(),
		)
	}
}

func bytesSliceToStrings(in [][]byte) []string {
	if in == nil {
		return nil
	}
	out := make([]string, len(in))
	for i, b := range in {
		out[i] = string(b)
	}
	return out
}

func (m *mergeFields) canMergeScalars(left, right *resolve.Field) bool {
	if left.OnTypeNames != nil && right.OnTypeNames != nil {
		if !m.sameOnTypeNames(left.OnTypeNames, right.OnTypeNames) {
			return false
		}
	}
	return true
}

func (m *mergeFields) mergeScalars(left, right *resolve.Field) {
	// when left has no type conditions, it will overwrite right
	if left.OnTypeNames == nil && left.ParentOnTypeNames == nil {
		return
	}
	// when right has no type conditions, it will overwrite left
	if right.OnTypeNames == nil && right.ParentOnTypeNames == nil {
		left.OnTypeNames = nil
		left.ParentOnTypeNames = nil
		return
	}
	left.OnTypeNames = m.deduplicateOnTypeNames(append(left.OnTypeNames, right.OnTypeNames...))

	// ParentOnTypeNames を Depth ごとに Names の union でマージする
	if len(left.ParentOnTypeNames) == 0 {
		left.ParentOnTypeNames = right.ParentOnTypeNames
		return
	}
	if len(right.ParentOnTypeNames) == 0 {
		return
	}

	depthToNames := make(map[int][][]byte, len(left.ParentOnTypeNames)+len(right.ParentOnTypeNames))
	for i := range left.ParentOnTypeNames {
		depth := left.ParentOnTypeNames[i].Depth
		depthToNames[depth] = append(depthToNames[depth], left.ParentOnTypeNames[i].Names...)
	}
	for i := range right.ParentOnTypeNames {
		depth := right.ParentOnTypeNames[i].Depth
		depthToNames[depth] = append(depthToNames[depth], right.ParentOnTypeNames[i].Names...)
	}

	merged := make([]resolve.ParentOnTypeNames, 0, len(depthToNames))
	for depth, names := range depthToNames {
		merged = append(merged, resolve.ParentOnTypeNames{
			Depth: depth,
			Names: m.deduplicateOnTypeNames(names),
		})
	}
	left.ParentOnTypeNames = merged
}

// canMergeArrays determines if two sibling array fields can be merged.
// Unlike scalars, we allow merging even if both sides have different OnTypeNames,
// because we will union the conditions to keep a single field and avoid dropping
// the key due to ordering/condition checks at execution time.
func (m *mergeFields) canMergeArrays(left, right *resolve.Field) bool {
	// Names and kinds are checked by the caller.
	// If both sides have explicit OnTypeNames, they must be identical; otherwise
	// we would mix selections that belong to different runtime types.
	if left.OnTypeNames != nil && right.OnTypeNames != nil {
		if !m.sameOnTypeNames(left.OnTypeNames, right.OnTypeNames) {
			return false
		}
	}
	return true
}

// mergeArrays merges two array fields into the left one, unifying type conditions
// and, if array items are objects, merging their fields as well.
func (m *mergeFields) mergeArrays(left, right *resolve.Field) {
	// when left has no type conditions, it will overwrite right (keep left as-is)
	if left.OnTypeNames == nil && left.ParentOnTypeNames == nil {
		// still merge inner object item fields if present to accumulate selections
		m.mergeValues(left, right)
		return
	}
	// when right has no type conditions, it will overwrite left
	if right.OnTypeNames == nil && right.ParentOnTypeNames == nil {
		left.OnTypeNames = nil
		left.ParentOnTypeNames = nil
		// still merge inner selections
		m.mergeValues(left, right)
		return
	}

	// union OnTypeNames
	left.OnTypeNames = m.deduplicateOnTypeNames(append(left.OnTypeNames, right.OnTypeNames...))

	// ParentOnTypeNames union per depth
	if len(left.ParentOnTypeNames) == 0 {
		left.ParentOnTypeNames = right.ParentOnTypeNames
	} else if len(right.ParentOnTypeNames) != 0 {
		depthToNames := make(map[int][][]byte, len(left.ParentOnTypeNames)+len(right.ParentOnTypeNames))
		for i := range left.ParentOnTypeNames {
			depth := left.ParentOnTypeNames[i].Depth
			depthToNames[depth] = append(depthToNames[depth], left.ParentOnTypeNames[i].Names...)
		}
		for i := range right.ParentOnTypeNames {
			depth := right.ParentOnTypeNames[i].Depth
			depthToNames[depth] = append(depthToNames[depth], right.ParentOnTypeNames[i].Names...)
		}
		merged := make([]resolve.ParentOnTypeNames, 0, len(depthToNames))
		for depth, names := range depthToNames {
			merged = append(merged, resolve.ParentOnTypeNames{
				Depth: depth,
				Names: m.deduplicateOnTypeNames(names),
			})
		}
		left.ParentOnTypeNames = merged
	}

	// merge inner object selections if necessary
	m.mergeValues(left, right)
}

func (m *mergeFields) fieldsCanMerge(left *resolve.Field, right *resolve.Field) bool {
	if !bytes.Equal(left.Name, right.Name) {
		return false
	}
	if left.Value.NodeKind() != right.Value.NodeKind() {
		return false
	}
	if !m.sameOnTypeNames(left.OnTypeNames, right.OnTypeNames) {
		return false
	}
	// scalars with different parent type conditions can't be merged at this point
	// we're handling this case later when we merge scalar fields
	if m.nodeIsScalar(left.Value) && !m.sameParentOnTypeNames(left, right) {
		return false
	}
	return true
}

func (m *mergeFields) deduplicateOnTypeNames(onTypeNames [][]byte) [][]byte {
	uniqueTypeNames := make(map[string]struct{}, len(onTypeNames))
	for _, typeName := range onTypeNames {
		uniqueTypeNames[string(typeName)] = struct{}{}
	}
	if len(uniqueTypeNames) == len(onTypeNames) {
		return onTypeNames
	}
	result := make([][]byte, 0, len(uniqueTypeNames))
	for typeName := range uniqueTypeNames {
		result = append(result, []byte(typeName))
	}
	return result
}

func (m *mergeFields) sameOnTypeNames(left, right [][]byte) bool {
	if len(left) != len(right) {
		return false
	}
WithNext:
	for i := range left {
		for j := range right {
			if bytes.Equal(left[i], right[j]) {
				continue WithNext
			}
		}
		return false
	}
	return true
}

func (m *mergeFields) sameParentOnTypeNames(left, right *resolve.Field) bool {
	if len(left.ParentOnTypeNames) != len(right.ParentOnTypeNames) {
		return false
	}
WithNext:
	for i := range left.ParentOnTypeNames {
		for j := range right.ParentOnTypeNames {
			if left.ParentOnTypeNames[i].Depth != right.ParentOnTypeNames[j].Depth {
				continue
			}
			if !m.sameOnTypeNames(left.ParentOnTypeNames[i].Names, right.ParentOnTypeNames[j].Names) {
				continue
			}
			// この depth レイヤーでマッチしたので次のレイヤーへ
			continue WithNext
		}
		// この depth レイヤーに対応する ParentOnTypeNames が right 側に存在しない
		return false
	}
	return true
}

func (m *mergeFields) mergeValues(left, right *resolve.Field) {
	switch l := left.Value.(type) {
	case *resolve.Object:
		r := right.Value.(*resolve.Object)
		l.Fields = append(l.Fields, r.Fields...)

		// オブジェクトフィールド同士の ParentOnTypeNames も Depth ごとに Names の union でマージする。
		// ただし、左側が無条件フィールド（自身に OnTypeNames も ParentOnTypeNames も無い）である場合は、
		// 右側の条件をフィールド自体に引き継がない。無条件フィールドが条件付きに変化してしまい、
		// ルートで選択されたサブフィールドが欠落する原因になるため。
		if !(left.OnTypeNames == nil && len(left.ParentOnTypeNames) == 0) && len(right.ParentOnTypeNames) != 0 {
			depthToNames := make(map[int][][]byte, len(left.ParentOnTypeNames)+len(right.ParentOnTypeNames))
			for i := range left.ParentOnTypeNames {
				depth := left.ParentOnTypeNames[i].Depth
				depthToNames[depth] = append(depthToNames[depth], left.ParentOnTypeNames[i].Names...)
			}
			for i := range right.ParentOnTypeNames {
				depth := right.ParentOnTypeNames[i].Depth
				depthToNames[depth] = append(depthToNames[depth], right.ParentOnTypeNames[i].Names...)
			}
			merged := make([]resolve.ParentOnTypeNames, 0, len(depthToNames))
			for depth, names := range depthToNames {
				merged = append(merged, resolve.ParentOnTypeNames{
					Depth: depth,
					Names: m.deduplicateOnTypeNames(names),
				})
			}
			left.ParentOnTypeNames = merged
		}
	case *resolve.Array:
		r := right.Value.(*resolve.Array)
		if l.Item.NodeKind() == resolve.NodeKindObject {
			lo := l.Item.(*resolve.Object)
			ro := r.Item.(*resolve.Object)
			lo.Fields = append(lo.Fields, ro.Fields...)
		}
		// 配列フィールド同士をマージする場合も、ParentOnTypeNames を Depth ごとに Names の union でマージする。
		// ただし、左側が無条件フィールドの場合は、右側の条件をフィールド自体に引き継がない（上記オブジェクトと同様）。
		if !(left.OnTypeNames == nil && len(left.ParentOnTypeNames) == 0) && len(right.ParentOnTypeNames) != 0 {
			depthToNames := make(map[int][][]byte, len(left.ParentOnTypeNames)+len(right.ParentOnTypeNames))
			for i := range left.ParentOnTypeNames {
				depth := left.ParentOnTypeNames[i].Depth
				depthToNames[depth] = append(depthToNames[depth], left.ParentOnTypeNames[i].Names...)
			}
			for i := range right.ParentOnTypeNames {
				depth := right.ParentOnTypeNames[i].Depth
				depthToNames[depth] = append(depthToNames[depth], right.ParentOnTypeNames[i].Names...)
			}
			merged := make([]resolve.ParentOnTypeNames, 0, len(depthToNames))
			for depth, names := range depthToNames {
				merged = append(merged, resolve.ParentOnTypeNames{
					Depth: depth,
					Names: m.deduplicateOnTypeNames(names),
				})
			}
			left.ParentOnTypeNames = merged
		}
	}
}

func (m *mergeFields) nodeIsScalar(node resolve.Node) bool {
	switch node.(type) {
	case *resolve.Object, *resolve.Array:
		return false
	}
	return true
}

func (m *mergeFields) propagateParentTypeNames(field *resolve.Field) {
	if field.OnTypeNames == nil {
		return
	}
	m.setParentTypeNames(field, field.OnTypeNames, 1)
}

// setParentTypeNames recursively sets the parent type names for all children of a field
// increasing the depth by 1 for each level
func (m *mergeFields) setParentTypeNames(field *resolve.Field, typeNames [][]byte, depth int) {
	switch field.Value.NodeKind() {
	case resolve.NodeKindObject:
		object := field.Value.(*resolve.Object)
		for i := range object.Fields {
			object.Fields[i].ParentOnTypeNames = append(object.Fields[i].ParentOnTypeNames, resolve.ParentOnTypeNames{
				Depth: depth,
				Names: typeNames,
			})
			m.setParentTypeNames(object.Fields[i], typeNames, depth+1)
		}
	case resolve.NodeKindArray:
		array := field.Value.(*resolve.Array)
		if array.Item.NodeKind() == resolve.NodeKindObject {
			object := array.Item.(*resolve.Object)
			for i := range object.Fields {
				object.Fields[i].ParentOnTypeNames = append(object.Fields[i].ParentOnTypeNames, resolve.ParentOnTypeNames{
					Depth: depth,
					Names: typeNames,
				})
				m.setParentTypeNames(object.Fields[i], typeNames, depth+1)
			}
		}
	}
}
