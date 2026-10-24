"""验证模板修复脚本的 XML 解析已禁用实体展开（XXE 加固回归测试）。

运行：python3 scripts/test_repair_income_templates.py
"""
import importlib.util
from pathlib import Path

from lxml import etree

SCRIPT = Path(__file__).with_name("repair_income_templates.py")


def load_module():
    spec = importlib.util.spec_from_file_location("repair_income_templates", SCRIPT)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def test_external_entity_is_not_expanded():
    """外部实体不得被展开，也不得读取宿主文件。"""
    module = load_module()
    payload = (
        b'<?xml version="1.0"?>'
        b'<!DOCTYPE root [<!ENTITY xxe SYSTEM "file:///etc/passwd">]>'
        b"<root>&xxe;</root>"
    )
    tree = etree.fromstring(payload, parser=module.SAFE_XML_PARSER)
    assert tree.text is None, "外部实体被展开：%r" % (tree.text,)
    assert b"root:" not in etree.tostring(tree), "实体内容进入了文档树"


def test_external_dtd_is_not_loaded():
    """外部 DTD 不得被加载：声明保留但解析成功，且不因取不到 DTD 而失败。"""
    module = load_module()
    payload = b'<?xml version="1.0"?><!DOCTYPE root SYSTEM "file:///etc/passwd"><root><a>ok</a></root>'
    tree = etree.fromstring(payload, parser=module.SAFE_XML_PARSER)
    assert tree.findtext("a") == "ok"


def test_well_formed_document_still_parses():
    """加固不能破坏正常模板解析。"""
    module = load_module()
    tree = etree.fromstring(b"<root><a>ok</a></root>", parser=module.SAFE_XML_PARSER)
    assert tree.findtext("a") == "ok"


if __name__ == "__main__":
    failures = 0
    for name, func in sorted(globals().items()):
        if name.startswith("test_") and callable(func):
            try:
                func()
                print("ok   %s" % name)
            except AssertionError as exc:
                failures += 1
                print("FAIL %s: %s" % (name, exc))
    raise SystemExit(1 if failures else 0)
