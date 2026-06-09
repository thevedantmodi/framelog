import xml.etree.ElementTree as ET
from pathlib import Path

from framelog.xmp import write_xmp

_RDF = "{http://www.w3.org/1999/02/22-rdf-syntax-ns#}"
_DC  = "{http://purl.org/dc/elements/1.1/}"


def _get_tags(sidecar: Path) -> list[str]:
    lines = [l for l in sidecar.read_text().splitlines() if not l.startswith("<?xpacket")]
    root = ET.fromstring("\n".join(lines))
    lis = root.findall(f".//{_RDF}Bag/{_RDF}li")
    return [li.text for li in lis]


def test_sidecar_path(tmp_path: Path):
    img = tmp_path / "DSF0001.RAF"
    img.write_bytes(b"fake")
    result = write_xmp(img, "2026-05-28T12:00:00", "X-T5")
    assert result == tmp_path / "DSF0001.xmp"
    assert result.exists()


def test_tags_present(tmp_path: Path):
    img = tmp_path / "DSF0001.RAF"
    img.write_bytes(b"fake")
    sidecar = write_xmp(img, "2026-05-28T12:00:00", "X-T5")
    tags = _get_tags(sidecar)
    assert "batch:2026-05-28T12:00:00" in tags
    assert "camera:X-T5" in tags


def test_camera_model_none(tmp_path: Path):
    img = tmp_path / "DSF0001.RAF"
    img.write_bytes(b"fake")
    sidecar = write_xmp(img, "2026-05-28T12:00:00", None)
    tags = _get_tags(sidecar)
    assert "camera:unknown" in tags


def test_overwrite(tmp_path: Path):
    img = tmp_path / "DSF0001.RAF"
    img.write_bytes(b"fake")
    write_xmp(img, "batch-1", "X-T5")
    write_xmp(img, "batch-2", "iPhone 15 Pro")
    tags = _get_tags(tmp_path / "DSF0001.xmp")
    cameras = [t for t in tags if t.startswith("camera:")]
    assert cameras == ["camera:iPhone 15 Pro"]
