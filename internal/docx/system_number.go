package docx

import (
	"fmt"
	"html"
	"strings"
)

// SystemContractNumberPending is frozen into drafts. Approval replaces only
// this reserved marker, never rerenders an administrator's mutable template.
const SystemContractNumberPending = "【系统合同编号：审批通过后生成】"

func ReplaceSystemContractNumber(document []byte, number string) ([]byte, error) {
	number = strings.TrimSpace(number)
	if number == "" || strings.ContainsAny(number, "\r\n") || strings.Contains(number, SystemContractNumberPending) {
		return nil, fmt.Errorf("invalid system contract number")
	}
	files, err := read(document)
	if err != nil {
		return nil, err
	}
	changed := false
	for name, body := range files {
		if !isWordXML(name) {
			continue
		}
		visible, nodes := visibleText(string(body))
		var offsets []int
		for cursor := 0; cursor < len(visible); {
			index := strings.Index(visible[cursor:], SystemContractNumberPending)
			if index < 0 {
				break
			}
			index += cursor
			offsets = append(offsets, index)
			cursor = index + len(SystemContractNumberPending)
		}
		for i := len(offsets) - 1; i >= 0; i-- {
			start, left := locate(nodes, offsets[i], false)
			end, right := locate(nodes, offsets[i]+len(SystemContractNumberPending), true)
			if start < 0 || end < 0 {
				return nil, fmt.Errorf("cannot locate system contract number")
			}
			if start == end {
				nodes[start].value = nodes[start].value[:left] + number + nodes[start].value[right:]
			} else {
				nodes[start].value = nodes[start].value[:left] + number
				for n := start + 1; n < end; n++ {
					nodes[n].value = ""
				}
				nodes[end].value = nodes[end].value[right:]
			}
		}
		if len(offsets) == 0 {
			continue
		}
		changed = true
		var out strings.Builder
		cursor := 0
		for _, node := range nodes {
			out.Write(body[cursor:node.contentStart])
			out.WriteString(html.EscapeString(node.value))
			cursor = node.contentEnd
		}
		out.Write(body[cursor:])
		files[name] = []byte(out.String())
	}
	if !changed {
		return append([]byte(nil), document...), nil
	}
	return write(document, files)
}
