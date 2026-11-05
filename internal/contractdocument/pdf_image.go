package contractdocument

import (
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/types"
)

// validatePDFImageBudget 在不解码或渲染图像的情况下约束图片几何与内存预算。
// 参数：table为解析后的引用表，stream包括页面图像、软遮罩及缩略图原始流。
// 返回值：非图片或预算内图片返回nil；维度非法、引用损坏或预算越界返回错误。
func validatePDFImageBudget(table *model.XRefTable, stream types.StreamDict) error {
	subtype, _, err := table.DereferenceNameEntry(stream.Dict, "Subtype")
	if err != nil {
		return Reject(CodePDFImageBudget, "PDF图片类型无效")
	}
	_, widthPresent := stream.Dict["Width"]
	_, heightPresent := stream.Dict["Height"]
	if (subtype == nil || *subtype != "Image") && !widthPresent && !heightPresent {
		return nil
	}
	width, err := table.DereferenceInteger(stream.Dict["Width"])
	if err != nil || width == nil || *width <= 0 {
		return Reject(CodePDFImageBudget, "PDF图片宽度无效")
	}
	height, err := table.DereferenceInteger(stream.Dict["Height"])
	if err != nil || height == nil || *height <= 0 {
		return Reject(CodePDFImageBudget, "PDF图片高度无效")
	}
	limits := table.Conf.Limits
	w, h := int64(*width), int64(*height)
	// 先除后乘，避免攻击者声明的巨型尺寸造成整数溢出。
	// 两条判断分别对应独立像素上限 MaxImagePixels 与 4 字节/像素渲染预算 MaxImageBytes/4；
	// 当前预算下 MaxImageBytes/4（192MiB/4≈50.3MP）先触发，A4@600dpi（34.8MP）通过、A3@600dpi（69.6MP）拒绝。
	if w > limits.MaxImagePixels/h || w > (limits.MaxImageBytes/4)/h {
		return Reject(CodePDFImageBudget, "PDF图片像素或渲染内存超出接入上限")
	}
	return validatePDFImageSampleBudget(table, &stream, w, h)
}

// validatePDFImageSampleBudget 约束解压像素行的内存，兼容遮罩和自描述JPX图片。
// 参数：table为引用表，stream为图片流，width及height为已验证的正数尺寸。
// 返回值：原始样本内存在上限内返回nil；无效引用或资源越界返回错误。
func validatePDFImageSampleBudget(table *model.XRefTable, stream *types.StreamDict,
	width, height int64) error {
	components, err := pdfImageColorComponents(table, stream.Dict["ColorSpace"],
		make(map[types.IndirectRef]bool), 0, true)
	if err != nil {
		return Reject(CodePDFImageBudget, "PDF图片色彩空间无效")
	}
	bits, err := table.DereferenceInteger(stream.Dict["BitsPerComponent"])
	if err != nil {
		return Reject(CodePDFImageBudget, "PDF图片采样精度无效")
	}
	mask, err := table.Dereference(stream.Dict["ImageMask"])
	if err != nil {
		return Reject(CodePDFImageBudget, "PDF图片遮罩声明无效")
	}
	precision := int64(16)
	if bits != nil {
		precision = int64(*bits)
	}
	if mask == types.Boolean(true) {
		components, precision = 1, 1
	}
	// JPX可省略采样和色彩声明；不启动编解码器，使用16位四通道保守预算。
	if components == 0 {
		components = 4
	}
	limit := table.Conf.Limits.MaxImageBytes
	if components < 1 || precision < 1 || precision > 16 ||
		int64(components) > (limit/height*8)/width/precision {
		return Reject(CodePDFImageBudget, "PDF图片解码内存超出接入上限")
	}
	return nil
}
