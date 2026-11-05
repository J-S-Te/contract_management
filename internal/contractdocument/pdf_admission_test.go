package contractdocument

import (
	"bytes"
	"compress/zlib"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// TestExternalPDFStaticActionWords 验证静态注释及文字不会被误判为活动对象。
// 参数：t为真实PDF准入测试上下文。
// 返回值：无；静态内容被拒绝或原件哈希变化时失败。
func TestExternalPDFStaticActionWords(t *testing.T) {
	content := "BT /F1 12 Tf 10 100 Td (/JS /JavaScript /EmbeddedFile /Launch /Encrypt) Tj ET"
	body := admissionPDF([]string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 200 200] " +
			"/Resources << /Font << /F1 4 0 R >> >> /Contents 5 0 R >>",
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
		fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(content), content),
	}, "% static /JS /JavaScript /EmbeddedFile /Launch /Encrypt\n")
	source, err := ValidateExternalSource("static.pdf", "application/pdf", body)
	if err != nil {
		t.Fatalf("合法静态内容被拒绝: %v", err)
	}
	if source.SHA256 != fmt.Sprintf("%x", sha256.Sum256(body)) || source.Text != "" {
		t.Fatal("静态原件被改写或虚构提取正文")
	}
}

// TestExternalPDFImageBudgets 验证无文字扫描件及图片像素和渲染预算。
// 参数：t为只做结构解析、不渲染图片的测试上下文。
// 返回值：无；600dpi扫描件误拒、A3@600dpi及以上误收或原因码变化时失败。
func TestExternalPDFImageBudgets(t *testing.T) {
	// 有效像素上限取 min(MaxImagePixels, MaxImageBytes/4)=min(64Mi,192MiB/4)=50331648≈50.3MP：
	// 8192×6144恰为上限、8193×6144越界；A4@600dpi(34.8MP)必须通过、A3@600dpi(69.6MP)必须拒绝。
	for _, sample := range []struct {
		name                                 string
		width, height, precision, components int
		colorSpace                           string
		indirect, reject                     bool
	}{
		{"scan", 32, 32, 8, 1, "/ColorSpace /DeviceGray", false, false},
		{"a4-600dpi", 4960, 7016, 8, 1, "/ColorSpace /DeviceGray", false, false},
		{"a3-600dpi", 7016, 9921, 8, 1, "/ColorSpace /DeviceGray", false, true},
		{"pixel-limit", 8192, 6144, 1, 1, "/ColorSpace /DeviceGray", false, false},
		{"pixel-over", 8193, 6144, 1, 1, "/ColorSpace /DeviceGray", false, true},
		{"indirect-over", 8193, 6144, 1, 1, "/ColorSpace /DeviceGray", true, true},
	} {
		t.Run(sample.name, func(t *testing.T) {
			body := admissionImagePDFSamples(t, sample.width, sample.height, sample.indirect,
				sample.precision, sample.components, sample.colorSpace)
			_, err := ValidateExternalSource("scan.pdf", "application/pdf", body)
			if sample.reject {
				requireRejectionCode(t, err, CodePDFImageBudget)
			} else if err != nil {
				t.Fatalf("正常扫描件被拒绝: %v", err)
			}
		})
	}
}

// TestExternalPDFCompressedStreamObjectBudget 验证多MB压缩图片流不被对象缓冲区上限误杀。
// 参数：t为真实结构解析上下文，夹具压缩后约5.7MB且不可再压缩。
// 返回值：无；编码流载荷被计入MaxObjectBytes或超过MaxStreamBytes时失败。
func TestExternalPDFCompressedStreamObjectBudget(t *testing.T) {
	body := admissionImagePDFNoise(t, 2400, 2400)
	if len(body) <= 4<<20 {
		t.Fatalf("夹具压缩后仅%d字节，未覆盖4MiB对象缓冲区上限", len(body))
	}
	if len(body) >= MaxPDFSize {
		t.Fatalf("夹具%d字节超出20MiB信封", len(body))
	}
	if _, err := ValidateExternalSource("noise.pdf", "application/pdf", body); err != nil {
		t.Fatalf("多MB压缩扫描流被误拒: %v", err)
	}
}

// TestExternalPDFSampleMemoryBudget 验证高精度色彩样本的原始行预算及遮罩兼容。
// 参数：t为离线准入测试上下文。
// 返回值：无；精确上限误拒、越界误收或合法遮罩不兼容时失败。
func TestExternalPDFSampleMemoryBudget(t *testing.T) {
	// 原始样本预算即 MaxImageBytes=192MiB：6144×4096@16bit CMYK 恰为201326592字节（等于上限）
	// 必须通过，多一行即越界；A4@600dpi 8bit CMYK（约139MB）通过、16bit CMYK（约278MB）拒绝。
	for _, sample := range []struct {
		name                                 string
		width, height, precision, components int
		colorSpace                           string
		reject                               bool
	}{
		{"raw-limit", 6144, 4096, 16, 4, "/ColorSpace /DeviceCMYK", false},
		{"raw-over", 6145, 4096, 16, 4, "/ColorSpace /DeviceCMYK", true},
		{"a4-600dpi-cmyk8", 4960, 7016, 8, 4, "/ColorSpace /DeviceCMYK", false},
		{"a4-600dpi-cmyk16", 4960, 7016, 16, 4, "/ColorSpace /DeviceCMYK", true},
		{"mask", 32, 32, 1, 1, "/ImageMask true", false},
	} {
		t.Run(sample.name, func(t *testing.T) {
			body := admissionImagePDFSamples(t, sample.width, sample.height, false,
				sample.precision, sample.components, sample.colorSpace)
			_, err := ValidateExternalSource("samples.pdf", "application/pdf", body)
			if sample.reject {
				requireRejectionCode(t, err, CodePDFImageBudget)
			} else if err != nil {
				t.Fatalf("合法样本或精确上限被拒绝: %v", err)
			}
		})
	}
}

// requireRejectionCode 断言准入结果是以指定原因码拒绝。
// 参数：t为测试上下文，err为准入返回错误，code为期望的稳定原因码。
// 返回值：无；未拒绝或原因码不符时失败。
func requireRejectionCode(t *testing.T, err error, code string) {
	t.Helper()
	if err == nil {
		t.Fatalf("超预算图片被接受，期望原因码%s", code)
	}
	var rejection *RejectionError
	if !errors.As(err, &rejection) || rejection.Code != code {
		t.Fatalf("原因码不符: 期望%s，实际%v", code, err)
	}
}

// TestExternalPDFUnreferencedImageBudget 验证未挂页面资源的图片也不能绕过预算。
// 参数：t为只进行结构检查的测试上下文。
// 返回值：无；独立图片对象超预算仍被接受时失败。
func TestExternalPDFUnreferencedImageBudget(t *testing.T) {
	body := admissionImagePDF(t, 8193, 6144, false)
	resource := []byte("/XObject << /Scan 4 0 R >>")
	// 同长空白删除资源引用，保留真实像素及全部xref偏移。
	body = bytes.Replace(body, resource, bytes.Repeat([]byte(" "), len(resource)), 1)
	_, err := ValidateExternalSource("unreferenced.pdf", "application/pdf", body)
	requireRejectionCode(t, err, CodePDFImageBudget)
}

// admissionImagePDF 生成有真实压缩像素且无文字层的静态扫描夹具。
// 参数：t为测试上下文，width和height为位图尺寸，indirect决定尺寸是否间接引用。
// 返回值：可结构解析的单页PDF；仅本地校验，不渲染或上传。
func admissionImagePDF(t *testing.T, width, height int, indirect bool) []byte {
	return admissionImagePDFSamples(t, width, height, indirect, 1, 1,
		"/ColorSpace /DeviceGray")
}

// admissionImagePDFSamples 为颜色精度及遮罩用例生成真实压缩扫描数据。
// 参数：t为测试上下文，其余参数决定尺寸引用、采样精度、通道数及颜色或遮罩声明。
// 返回值：合法像素流与交叉引用齐备的单页PDF；不调用图像编解码或渲染器。
func admissionImagePDFSamples(t *testing.T, width, height int, indirect bool,
	precision, components int, colorSpace string) []byte {
	t.Helper()
	return admissionImagePDFRows(t, width, height, indirect, precision, components, colorSpace,
		func([]byte) {})
}

// admissionImagePDFNoise 生成压缩后仍达数MB的不可压缩像素流夹具。
// 参数：t为测试上下文，width和height为位图尺寸，每行用确定性伪随机数填充。
// 返回值：可准入的单页PDF；用于核实对象缓冲区上限不作用于编码流载荷。
func admissionImagePDFNoise(t *testing.T, width, height int) []byte {
	t.Helper()
	state := uint64(0x9e3779b97f4a7c15)
	return admissionImagePDFRows(t, width, height, false, 8, 1, "/ColorSpace /DeviceGray",
		func(row []byte) {
			for index := range row {
				state ^= state << 13
				state ^= state >> 7
				state ^= state << 17
				row[index] = byte(state)
			}
		})
}

// admissionImagePDFRows 按行生成像素流并组装单页静态扫描夹具。
// 参数：t为测试上下文，其余参数决定尺寸引用、采样精度、通道数、颜色声明及逐行填充函数。
// 返回值：合法像素流与交叉引用齐备的单页PDF；不渲染、不改写调用方数据。
func admissionImagePDFRows(t *testing.T, width, height int, indirect bool,
	precision, components int, colorSpace string, fill func([]byte)) []byte {
	t.Helper()
	var pixels bytes.Buffer
	compressed := zlib.NewWriter(&pixels)
	row := make([]byte, (width*components*precision+7)/8)
	for index := 0; index < height; index++ {
		fill(row)
		if _, err := compressed.Write(row); err != nil {
			t.Fatal(err)
		}
	}
	if err := compressed.Close(); err != nil {
		t.Fatal(err)
	}
	dimensions := fmt.Sprintf("/Width %d /Height %d", width, height)
	if indirect {
		dimensions = "/Width 6 0 R /Height 7 0 R"
	}
	content := "q 200 0 0 200 0 0 cm /Scan Do Q"
	objects := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 200 200] " +
			"/Resources << /XObject << /Scan 4 0 R >> >> /Contents 5 0 R >>",
		fmt.Sprintf("<< /Type /XObject /Subtype /Image %s %s "+
			"/BitsPerComponent %d /Filter /FlateDecode /Length %d >>\nstream\n%s\nendstream",
			dimensions, colorSpace, precision, pixels.Len(), pixels.String()),
		fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(content), content),
	}
	if indirect {
		objects = append(objects, fmt.Sprint(width), fmt.Sprint(height))
	}
	return admissionPDF(objects, "")
}

// admissionPDF 为本包夹具建立准确交叉引用及合法注释位置。
// 参数：objects为按对象号排列的静态对象，comment为对象前的PDF注释。
// 返回值：保留原始对象和流字节的完整PDF。
func admissionPDF(objects []string, comment string) []byte {
	var output strings.Builder
	output.WriteString("%PDF-1.7\n" + comment)
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
	fmt.Fprintf(&output, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF",
		len(objects)+1, start)
	return []byte(output.String())
}
