"""Build corrected copies without touching the supplied DOCX originals.

Only explicit, reviewed text spans change. Signature lines and other Zip parts
are retained. Payment-body runs override accidental 36pt inherited styles;
this does not introduce payment business models.
Run with the bundled artifact Python runtime.
"""
from pathlib import Path
from zipfile import ZipFile
import re
from lxml import etree

NS = {"w": "http://schemas.openxmlformats.org/wordprocessingml/2006/main"}
ROOT = Path(__file__).resolve().parents[1]
SOURCE = ROOT / "收入合同模版"
DEST = SOURCE / "修正版"


def text(p):
    return "".join(p.xpath(".//w:t/text()", namespaces=NS))


def replace_span(p, start, end, replacement):
    nodes = p.xpath(".//w:t", namespaces=NS)
    offsets, cursor = [], 0
    for node in nodes:
        value = node.text or ""
        offsets.append((node, cursor, cursor + len(value)))
        cursor += len(value)
    first = next((i for i, (_, a, b) in enumerate(offsets) if a <= start < b), None)
    last = next((i for i, (_, a, b) in enumerate(offsets) if a < end <= b), None)
    if first is None or last is None:
        raise ValueError("cannot locate reviewed replacement")
    node, a, _ = offsets[first]
    tail, ta, _ = offsets[last]
    if first == last:
        node.text = (node.text or "")[:start-a] + replacement + (node.text or "")[end-a:]
    else:
        node.text = (node.text or "")[:start-a] + replacement
        for i in range(first+1, last):
            offsets[i][0].text = ""
        tail.text = (tail.text or "")[end-ta:]
    node.set("{http://www.w3.org/XML/1998/namespace}space", "preserve")


def sub(p, pattern, replacement, count=1):
    matches = list(re.finditer(pattern, text(p)))
    if len(matches) != count:
        raise ValueError(f"expected {count} matches for {pattern!r}, got {len(matches)}: {text(p)!r}")
    for match in reversed(matches):
        replace_span(p, match.start(), match.end(), replacement)


def payment_blanks(p, first):
    # Existing clauses retain their original payment-choice wording.
    sub(p, r"RMB¥\s*_?元", "{{"+first+"}}元")
    sub(p, r"人民币\s*元整", "{{金额_大写 "+first+"}}")


def correct(index, tree):
    ps = tree.xpath("//w:p", namespaces=NS)
    if index in (1, 2, 3):
        # These body clauses inherited a 36pt cover-title style in the supplied
        # documents. Set only the reviewed payment block to normal 12pt body
        # size, leaving covers and all other paragraph styles untouched.
        body_rows = {1: range(42, 47), 2: range(126, 139), 3: range(53, 59)}[index]
        for row in body_rows:
            for run in ps[row].xpath("./w:r", namespaces=NS):
                props = run.find("w:rPr", NS)
                if props is None:
                    props = etree.Element("{"+NS["w"]+"}rPr")
                    run.insert(0, props)
                for tag in ("sz", "szCs"):
                    size = props.find("w:"+tag, NS)
                    if size is None:
                        size = etree.SubElement(props, "{"+NS["w"]+"}"+tag)
                    size.set("{"+NS["w"]+"}val", "24")
        payment_rows = {1: (43,45,46), 2: (129,131,132), 3: (55,57,58)}[index]
        for row, field in zip(payment_rows, ["付款金额","首付款金额","尾款金额"]):
            payment_blanks(ps[row], field)
        sub(ps[payment_rows[0]], r"\{\{付款比例\}\}(?!%)", "{{付款比例}}%")
        if index == 1:
            sub(ps[45], r"合同额\s*%", "合同额{{首付款比例}}%")
            sub(ps[46], r"剩余\s*%", "剩余{{尾款比例}}%")
            sub(ps[38], r"\{\{签订日期\}\}起至\s*\{\{签订日期\}\}", "{{服务期限起}}起至{{服务期限止}}")
            sub(ps[41], r"\}\}）元整", "}}）。")
            sub(ps[60], r"\{\{客户开户行账号\}\}", "{{我方开户行账号}}")
        if index in (2, 3):
            row = 126 if index == 2 else 52
            sub(ps[row], r"\{\{合同金额\}\}元整", "{{金额_大写 合同金额}}")
            sub(ps[row], r"\{\{付款金额\}\}", "{{合同金额}}")
        if index == 2:
            sub(ps[40], r"就\s*_（", "就{{客户名称}}（")
            sub(ps[40], r"委托\{\{客户名称\}\}", "委托{{我方名称}}")
            sub(ps[43], r"完成\s*（项目名称）", "完成{{项目名称}}（项目名称）")
            sub(ps[86], r"\{\{签订日期\}\}", "{{服务期限止}}")
            sub(ps[86], r"（\s*）", "（{{履行地点}}）")
            for row, side in [(89,"客户"),(90,"我方")]:
                sub(ps[row], r"联系人：[ ]+", "联系人：{{"+side+"联系人}}")
                sub(ps[row], r"联系电话[:：]?[ ]+(?=；)", "联系电话：{{"+side+"联系电话}}")
                sub(ps[row], r"项目经理[:：]?[ ]+", "项目经理：{{"+side+"项目联系人}}")
                sub(ps[row], r"联系电话[:：]?[ ]+(?=。)", "联系电话：{{"+side+"项目联系电话}}")
            sub(ps[124], r"\{\{签订日期\}\}", "{{成果提交日期}}")
            sub(ps[171], r"[ ]+（乙方）", "{{我方名称}}（乙方）")
            sub(ps[171], r"为[ ]+（项目名称）", "为{{项目名称}}（项目名称）")
            sub(ps[201], r"日期：[ ]+", "日期：{{签订日期}}  ", 1)
            sub(ps[201], r"日期：$", "日期：{{签订日期}}")
        if index == 3:
            for row,side in [(114,"客户"),(115,"我方"),(168,"客户"),(169,"我方")]:
                sub(ps[row], r"：[ ]+", "：{{"+side+"名称}}")
            sub(ps[187], r"甲方：[ ]+", "甲方：{{客户名称}}  ")
            sub(ps[187], r"乙方：[ ]+", "乙方：{{我方名称}}")
            for row in (157,160):
                sub(ps[row], r"日期：[ ]+", "日期：{{签订日期}}")
    if index == 4:
        sub(ps[0], r"\{\{合同编号\}\}", "{{系统合同编号}}")
        sub(ps[41], r"在[ ]+签订", "在{{签订地点}}签订")
        sub(ps[43], r"：[ ]+", "：{{客户名称}}")
        sub(ps[97], r"\{\{不含税金额_大写\}\}", "{{金额_大写 不含税金额}}")
        sub(ps[97], r"人民币大写：[ ]+", "人民币大写：{{金额_大写 增值税金额}}")
        sub(ps[97], r"小写：[ ]+元", "小写：{{增值税金额}}元")
        sub(ps[97], r"\{\{合同金额_大写\}\}元整", "{{金额_大写 合同金额}}")
        sub(ps[124], r"\{\{首付款金额_大写\}\}元整", "{{金额_大写 首付款金额}}")
        sub(ps[92], r"第[ ]+款", "第{{服务费计算方式}}款")
        sub(ps[120], r"按照[ ]+款", "按照{{付款方式条款}}款")
        sub(ps[139], r"\}\}300 1705 2080 5050 3580", "}}")
        sub(ps[198], r"14.2.1[ ]+", "14.2.1 {{客户名称}}")
        sub(ps[387], r"与[ ]+于", "与{{客户名称}}于")
        sub(ps[387], r"《[ ]+》", "《{{项目名称}}》")
        sub(ps[288], r"甲方（签章）：[ ]+", "甲方（签章）：{{客户名称}}  ")
        sub(ps[288], r"乙方（签章）：[ ]+", "乙方（签章）：{{我方名称}}")
    if index == 5:
        covers = [p for p in ps if text(p).strip() == "2026年7月"]
        if len(covers) != 1:
            raise ValueError("expected unique fixed cover date")
        sub(covers[0], "2026年7月", "{{签订日期}}")
        sub(ps[65], r"\{\{金额_大写 合同金额\}\}元整", "{{金额_大写 合同金额}}")


def main():
    sources = sorted(SOURCE.glob("[1-5]、*.docx"))
    if len(sources) != 5:
        raise ValueError("expected exactly five original income templates")
    DEST.mkdir(exist_ok=True)
    for index, source in enumerate(sources, 1):
        target = DEST / source.name
        with ZipFile(source) as original:
            tree = etree.fromstring(original.read("word/document.xml"))
            correct(index, tree)
            body = etree.tostring(tree, encoding="UTF-8", xml_declaration=True, standalone=True)
            with ZipFile(target, "w") as output:
                for entry in original.infolist():
                    output.writestr(entry, body if entry.filename == "word/document.xml" else original.read(entry.filename))
        print(target.name)


if __name__ == "__main__":
    main()
