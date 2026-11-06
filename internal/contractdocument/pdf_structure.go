package contractdocument

import (
	"bytes"
	"context"
	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/types"
	"time"
)

var pdfValidationSlots = make(chan struct{}, 2)

// validatePDFStructure 离线解析交叉引用、对象流和页面树，不改写原件及数字签名。
// 参数：body为已检查20MiB信封的PDF，解析最多五秒、两个并发。
// 返回值：有效静态文档为nil；损坏、活动对象、加密或资源越界返回中文错误。
func validatePDFStructure(body []byte) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	select {
	case pdfValidationSlots <- struct{}{}:
		defer func() { <-pdfValidationSlots }()
	case <-ctx.Done():
		return RejectRetryable(CodePDFValidationBusy, "PDF校验繁忙，请稍后重试")
	}
	conf := model.NewStatelessConfiguration()
	// 准入用 relaxed：PDF 规范的元数据苛求（如带 /PieceInfo 就必须有 /ModDate）与安全无关，
	// 而 WPS/金山等打印管道导出的业务 PDF 普遍是空 Info 字典 + /PieceInfo，严格模式会把整份合法
	// 原件误拒成 422。安全性仍由本包独立闸门承担：Encrypt 检查、forbiddenPDFName 活动对象检查、
	// 图片/色彩/对象预算，以及下面的 PageCount>=1；relaxed 只把常见的规范偏差降级为 repair 记录。
	conf.Cmd, conf.ValidationMode, conf.Offline = model.VALIDATE, model.ValidationRelaxed, true
	conf.ValidateLinks, conf.Optimize, conf.DecodeAllStreams = false, false, false
	// 图片预算按 600dpi 扫描件设计：A4@600dpi 灰度/彩色 8bit 为 4960×7016≈34.8MP，必须通过；
	// A3@600dpi（7016×9921≈69.6MP）及更大必须拒绝，因此单图有效像素上限卡在约 50MP 量级。
	// MaxImageBytes、MaxImagePixels、MaxDecodeBytes 的关系与本配置下谁先触发：
	//  1) pdf_image.go 的每图有效像素上限 = min(MaxImagePixels, MaxImageBytes/4)
	//     = min(64Mi=67108864, 192MiB/4=50331648)，即 50.3MP 先触发，对应 4 字节/像素渲染预算；
	//  2) MaxImagePixels=64Mi(67.1MP) 是独立像素上限，只有放宽 MaxImageBytes 时才会先触发；
	//  3) MaxImageBytes=192MiB(201MB) 限制解压后原始样本字节（宽×高×通道×位深/8）：
	//     通过上限的 A4@600 单图最大解码量约 139MB（8bit CMYK），16bit 灰度约 69.6MB；
	//     16bit CMYK A4@600 约 278MB，按字节上限拒绝；
	//  4) MaxDecodeBytes 限制单流解码产物总量，取与 MaxImageBytes 相同的 192MiB，避免过滤器解压后
	//     仍被更小的解码上限二次误伤。
	// MaxObjectBytes 只作用在间接对象字典缓冲区，不含流载荷（pdfcpu model.ResourceLimits 注释：
	// "limits an indirect-object buffer, excluding stream payloads"；read.go 的 buffer() 在 stream
	// 关键字处停止增长），所以 2–8MB 的压缩扫描流不会被它误杀，编码流体积由 MaxStreamBytes=MaxPDFSize 兜住。
	conf.Limits = model.ResourceLimits{MaxInputBytes: MaxPDFSize, MaxObjectBytes: 4 << 20,
		MaxStreamBytes: MaxPDFSize, MaxDecodeBytes: 192 << 20, MaxImageBytes: 192 << 20,
		MaxImagePixels: 64 << 20,
		MaxObjectCount: 100000, MaxObjectStreamCount: 20000, MaxObjectStreamFirst: 1 << 20,
		MaxXRefEntries: 100000, MaxRecursionDepth: 64}
	parsed, err := api.ReadContext(ctx, bytes.NewReader(body), conf)
	if err != nil {
		return Reject(CodePDFStructureInvalid, "PDF结构损坏、加密或超出解析资源上限")
	}
	if parsed.Encrypt != nil {
		return Reject(CodePDFEncrypted, "PDF含不支持的加密内容")
	}
	budget := 1000000
	// 先检查解码后对象，避免校验器忽略未知键或修复字典后遗漏活动内容。
	for _, entry := range parsed.Table {
		if entry == nil || entry.Free {
			continue
		}
		if err = inspectPDFObject(entry.Object, 0, &budget); err != nil {
			return err
		}
		if stream, ok := entry.Object.(types.StreamDict); ok {
			if err = validatePDFImageBudget(parsed.XRefTable, stream); err != nil {
				return err
			}
		}
	}
	if err = api.ValidateContext(ctx, parsed); err != nil || parsed.PageCount < 1 {
		return Reject(CodePDFPageStructure, "PDF页面结构不完整或不符合接入要求")
	}
	return nil
}

// forbiddenPDFName 对解码后的字典名及动作名执行统一准入策略。
// 参数：name为PDF名称对象的原始或已解码值。
// 返回值：禁止特征或非法名称为true。
// 除脚本/嵌入/启动/加密外，这里同时拦截外部跳转动作（URI/GoToR/GoToE）、
// 表单提交与数据导入（SubmitForm/ImportData）、附加动作与打开动作（AA/OpenAction）
// 以及多媒体动作（Rendition/Sound/Movie）：它们都会让静态合同在阅读器中触发外部
// 访问或非预期副作用，因此一律按活动内容拒绝。名称比较沿用PDF规范的大小写形式。
func forbiddenPDFName(name string) bool {
	decoded, err := types.DecodeName(name)
	if err != nil {
		return true
	}
	switch decoded {
	case "JavaScript", "JS", "EmbeddedFile", "EmbeddedFiles", "Launch", "Encrypt",
		"URI", "GoToR", "GoToE", "SubmitForm", "ImportData", "AA", "OpenAction",
		"Rendition", "Sound", "Movie":
		return true
	}
	return false
}

// inspectPDFObject 有界遍历解码对象，覆盖名称转义及压缩对象流。
// 参数：object为解析对象，depth为嵌套深度，budget限制总节点数。
// 返回值：禁止对象或资源越界错误；不解引用循环引用、不执行任何动作。
func inspectPDFObject(object types.Object, depth int, budget *int) error {
	*budget--
	if *budget < 0 || depth > 64 {
		return Reject(CodePDFObjectBudget, "PDF对象数量或嵌套超出接入上限")
	}
	switch value := object.(type) {
	case types.Name:
		if forbiddenPDFName(string(value)) {
			return Reject(CodePDFActiveContent, "PDF含不支持的活动、嵌入或加密内容")
		}
	case types.Dict:
		for key, child := range value {
			if forbiddenPDFName(key) {
				return Reject(CodePDFActiveContent, "PDF含不支持的活动、嵌入或加密内容")
			}
			if err := inspectPDFObject(child, depth+1, budget); err != nil {
				return err
			}
		}
	case types.Array:
		for _, child := range value {
			if err := inspectPDFObject(child, depth+1, budget); err != nil {
				return err
			}
		}
	case types.StreamDict:
		return inspectPDFObject(value.Dict, depth+1, budget)
	case types.ObjectStreamDict:
		if err := inspectPDFObject(value.Dict, depth+1, budget); err != nil {
			return err
		}
		return inspectPDFObject(value.ObjArray, depth+1, budget)
	}
	return nil
}
