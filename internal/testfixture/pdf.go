package testfixture

import (
	"fmt"
	"strings"
)

// PDF 生成含页面树、字体、内容流和准确交叉引用的真实测试原件。
// 参数：text为测试页面文本，仅用于非敏感夹具。
// 返回值：可被结构解析器和浏览器读取的一页PDF。
func PDF(text string) []byte {
	return PDFWithPadding(text, 0)
}

// PDFWithPadding 在首个对象前添加合法注释，生成大文件边界夹具。
// 参数：text为页面文本，padding为注释填充字节数。
// 返回值：具有正确交叉引用及尾部定位的一页PDF。
func PDFWithPadding(text string, padding int) []byte {
	return pdfDocument(text, padding, "")
}

// PDFAction 生成具有名称转义动作的真实结构样本。
// 参数：action为PDF动作名称，允许测试名称转义。
// 返回值：含活动对象的负向准入夹具，不得用于业务成功样本。
func PDFAction(action string) []byte { return pdfDocument("unsafe", 0, action) }

// pdfDocument 按明确对象列表生成测试文件，避免伪信封替代结构测试。
// 参数：text为页面文本，padding为注释填充，action为空或负向动作名。
// 返回值：带准确交叉引用的PDF字节。
func pdfDocument(text string, padding int, action string) []byte {
	text = strings.NewReplacer("\\", "\\\\", "(", "\\(", ")", "\\)").Replace(text)
	content := "BT /F1 12 Tf 50 300 Td (" + text + ") Tj ET\n"
	objects := []string{"<< /Type /Catalog /Pages 2 0 R >>", "<< /Type /Pages /Kids [3 0 R] /Count 1 >>", "<< /Type /Page /Parent 2 0 R /MediaBox [0 0 400 400] /Resources << /Font << /F1 4 0 R >> >> /Contents 5 0 R >>", "<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>", fmt.Sprintf("<< /Length %d >>\nstream\n%sendstream", len(content), content)}
	if action != "" {
		objects[0] = "<< /Type /Catalog /Pages 2 0 R /OpenAction 6 0 R >>"
		objects = append(objects, "<< /S /"+action+" /J#53 (app.alert\\(1\\)) >>")
	}
	var output strings.Builder
	output.WriteString("%PDF-1.4\n")
	if padding > 0 {
		output.WriteString("%")
		output.WriteString(strings.Repeat("P", padding))
		output.WriteString("\n")
	}
	offsets := []int{0}
	for index, object := range objects {
		offsets = append(offsets, output.Len())
		fmt.Fprintf(&output, "%d 0 obj\n%s\nendobj\n", index+1, object)
	}
	start := output.Len()
	fmt.Fprintf(&output, "xref\n0 %d\n0000000000 65535 f \n", len(objects)+1)
	for _, offset := range offsets[1:] {
		fmt.Fprintf(&output, "%010d 00000 n \n", offset)
	}
	fmt.Fprintf(&output, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF", len(objects)+1, start)
	return []byte(output.String())
}
