"""Generate local-compatible copies; preserve originals and non-document ZIP parts.

Run with the bundled document Python runtime. No application/network access.
"""
from pathlib import Path
from zipfile import ZipFile
import hashlib
import copy
import re
from lxml import etree
from repair_income_templates import NS, SAFE_XML_PARSER, text, replace_span

ROOT = Path(__file__).resolve().parents[1]
SOURCE = ROOT / "收入合同模版/宁波合同模板"
DEST = SOURCE / "系统适配版"


def adapt(source):
    before = hashlib.sha256(source.read_bytes()).hexdigest()
    with ZipFile(source) as original:
        tree = etree.fromstring(original.read("word/document.xml"), parser=SAFE_XML_PARSER)
        tables = tree.xpath("//w:tbl", namespaces=NS)
        rows = tables[0].xpath("./w:tr", namespaces=NS)[2:]
        if len(rows) != 16:
            raise ValueError("Expected exactly 16 service rows")
        row_fields = {"系统名称", "系统等级", "单价", "合计", "属地简称", "被服务单位名称"}
        for index, row in enumerate(rows, 1):
            for p in row.xpath(".//w:p", namespaces=NS):
                for m in reversed(list(re.finditer(r"\{\{([^{}]+)\}\}", text(p)))):
                    name = m[1]
                    if name not in row_fields:
                        raise ValueError(f"Unexpected service placeholder {name}")
                    default = "0" if name in {"单价", "合计"} else "不适用"
                    replace_span(p, m.start(), m.end(), "{{服务%02d_%s '%s'}}" % (index, name, default))
        checks = 0
        numbers = 0
        money = 0
        for p in tree.xpath("//w:p", namespaces=NS):
            for m in reversed(list(re.finditer(r"\{\{check '([^']+)'\}\}", text(p)))):
                replace_span(p, m.start(), m.end(), "{{选项_%s '□'}} %s" % (m[1], m[1]))
                checks += 1
            for m in reversed(list(re.finditer(r"\{\{合同编号\}\}", text(p)))):
                replace_span(p, m.start(), m.end(), "{{系统合同编号}}")
                numbers += 1
            for m in reversed(list(re.finditer(r"(\{\{金额_大写 合同金额\}\})\s*元整", text(p)))):
                replace_span(p, m.start(), m.end(), m[1])
                money += 1
        if (checks, numbers, money) != (16, 2, 1):
            raise ValueError(f"Source changed: checks/numbers/money = {checks}/{numbers}/{money}")
        def ensure(parent, tag):
            child = parent.find("w:" + tag, NS)
            if child is None:
                child = etree.SubElement(parent, "{" + NS["w"] + "}" + tag)
            return child
        for ti, table in enumerate(tables):
            if ti == 4:
                # Floating positioning separates the checklist heading from its
                # body in generated documents; keep the same table inline.
                floating = table.find("w:tblPr/w:tblpPr", NS)
                if floating is not None:
                    floating.getparent().remove(floating)
            trs = table.findall("w:tr", NS)
            for ri, row in enumerate(trs):
                props = row.find("w:trPr", NS)
                if props is None:
                    props = etree.Element("{" + NS["w"] + "}trPr")
                    row.insert(0, props)
                ensure(props, "cantSplit")
                if ti == 0 and ri < 2:
                    ensure(props, "tblHeader")
                # Keep signing blocks and the short industry checklist together.
                if ti in (3, 4, 5) and ri < len(trs) - 1:
                    for p in row.xpath(".//w:p", namespaces=NS):
                        pp = p.find("w:pPr", NS)
                        if pp is None:
                            pp = etree.Element("{" + NS["w"] + "}pPr")
                            p.insert(0, pp)
                        ensure(pp, "keepNext")
            if ti in (3, 5):
                caption = table.getprevious()
                while caption is not None and caption.tag == "{" + NS["w"] + "}p":
                    if text(caption).strip() not in ("本页无正文", "盖章页"):
                        break
                    pp = caption.find("w:pPr", NS)
                    if pp is None:
                        pp = etree.Element("{" + NS["w"] + "}pPr")
                        caption.insert(0, pp)
                    ensure(pp, "keepNext")
                    caption = caption.getprevious()
        # Empty cover spacer paragraphs inherited the large title's line height.
        # Keep the paragraphs/section break but reserve room for the party block.
        for p in tree.xpath("/w:document/w:body/w:p", namespaces=NS):
            if text(p).startswith("委托方"):
                break
            if not text(p).strip():
                spacing = p.find("w:pPr/w:spacing", NS)
                if spacing is not None:
                    for key, value in {"before": "0", "after": "0", "line": "360", "lineRule": "exact"}.items():
                        spacing.set("{" + NS["w"] + "}" + key, value)
        target = DEST / source.name.replace("_改造版", "_系统适配版")
        body = etree.tostring(tree, encoding="UTF-8", xml_declaration=True, standalone=True)
        # Idempotent regeneration is allowed only when the existing output matches.
        if target.exists():
            with ZipFile(target) as previous:
                if previous.read("word/document.xml") != body:
                    raise ValueError(f"Refusing to overwrite changed output: {target}")
            print("UNCHANGED", target.name)
            return
        DEST.mkdir(parents=True, exist_ok=True)
        with ZipFile(target, "w") as output:
            for entry in original.infolist():
                output.writestr(copy.copy(entry), body if entry.filename == "word/document.xml" else original.read(entry.filename))
        with ZipFile(target) as output:
            assert original.namelist() == output.namelist()
            for name in original.namelist():
                if name != "word/document.xml":
                    assert original.read(name) == output.read(name), name
    assert hashlib.sha256(source.read_bytes()).hexdigest() == before
    print("CREATED", target.name, "original SHA256", before)


if __name__ == "__main__":
    sources = sorted(SOURCE.glob("*.docx"))
    if len(sources) != 2:
        raise ValueError("Expected two original Ningbo templates")
    for source in sources:
        adapt(source)
