"""验证宁波模板适配脚本复用加固 XML 解析器（XXE 加固回归测试）。

与 test_repair_income_templates.py 同一模式：适配脚本必须复用
repair_income_templates.SAFE_XML_PARSER（实体不展开、不加载外部 DTD），
不得回落到 lxml 默认解析配置。

运行：uv run --with lxml python scripts/test_adapt_ningbo_templates.py
"""
import importlib.util
import sys
from pathlib import Path

from lxml import etree

HERE = Path(__file__).parent
REPAIR = HERE / "repair_income_templates.py"
ADAPT = HERE / "adapt_ningbo_templates.py"


def load_modules():
    def load(name, path):
        spec = importlib.util.spec_from_file_location(name, path)
        module = importlib.util.module_from_spec(spec)
        sys.modules[name] = module
        spec.loader.exec_module(module)
        return module

    # adapt_ningbo_templates 顶层 `from repair_income_templates import ...`
    # 依赖同目录模块已注册到 sys.modules，必须先加载 repair。
    repair = load("repair_income_templates", REPAIR)
    adapt = load("adapt_ningbo_templates", ADAPT)
    return repair, adapt


def test_adapt_reuses_hardened_parser():
    """适配脚本不得自建解析器，必须与修复脚本共用同一加固实例。"""
    repair, adapt = load_modules()
    assert adapt.SAFE_XML_PARSER is repair.SAFE_XML_PARSER, (
        "adapt_ningbo_templates 未复用 repair_income_templates 的 SAFE_XML_PARSER"
    )


def test_external_entity_is_not_expanded():
    """外部实体不得被展开，也不得读取宿主文件。"""
    _, adapt = load_modules()
    payload = (
        b'<?xml version="1.0"?>'
        b'<!DOCTYPE root [<!ENTITY xxe SYSTEM "file:///etc/passwd">]>'
        b"<root>&xxe;</root>"
    )
    tree = etree.fromstring(payload, parser=adapt.SAFE_XML_PARSER)
    assert tree.text is None, "外部实体被展开：%r" % (tree.text,)
    assert b"root:" not in etree.tostring(tree), "实体内容进入了文档树"


def test_external_dtd_is_not_loaded():
    """外部 DTD 不得被加载：声明保留但解析成功，且不因取不到 DTD 而失败。"""
    _, adapt = load_modules()
    payload = b'<?xml version="1.0"?><!DOCTYPE root SYSTEM "file:///etc/passwd"><root><a>ok</a></root>'
    tree = etree.fromstring(payload, parser=adapt.SAFE_XML_PARSER)
    assert tree.findtext("a") == "ok"


def test_well_formed_document_still_parses():
    """加固不能破坏正常模板解析。"""
    _, adapt = load_modules()
    tree = etree.fromstring(b"<root><a>ok</a></root>", parser=adapt.SAFE_XML_PARSER)
    assert tree.findtext("a") == "ok"


if __name__ == "__main__":
    failures = 0
    for name, func in sorted(globals().items()):
        if name.startswith("test_") and callable(func):
            try:
                func()
                print("ok   %s" % name)
            except Exception as exc:  # noqa: BLE001 — 红绿判定需要看到任何失败形态
                failures += 1
                print("FAIL %s: %s: %s" % (name, type(exc).__name__, exc))
    raise SystemExit(1 if failures else 0)
