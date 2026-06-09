import xml.etree.ElementTree as ET
from pathlib import Path

_NS = {
    "x":   "adobe:ns:meta/",
    "rdf": "http://www.w3.org/1999/02/22-rdf-syntax-ns#",
    "crs": "http://ns.adobe.com/camera-raw-settings/1.0/",
    "dc":  "http://purl.org/dc/elements/1.1/",
}

for _prefix, _uri in _NS.items():
    ET.register_namespace(_prefix, _uri)

_X   = "{adobe:ns:meta/}"
_RDF = "{http://www.w3.org/1999/02/22-rdf-syntax-ns#}"
_DC  = "{http://purl.org/dc/elements/1.1/}"


def write_xmp(
    image_path: Path,
    batch_id: str,
    camera_model: str | None,
) -> Path:
    """Write an XMP sidecar next to image_path with source, batch, and camera tags.

    Overwrites any existing sidecar. Returns the path to the .xmp file.
    """
    xmpmeta = ET.Element(f"{_X}xmpmeta")
    rdf = ET.SubElement(xmpmeta, f"{_RDF}RDF")
    desc = ET.SubElement(rdf, f"{_RDF}Description", {
        f"{_RDF}about": "",
    })
    subject = ET.SubElement(desc, f"{_DC}subject")
    bag = ET.SubElement(subject, f"{_RDF}Bag")

    tags = [
        f"batch:{batch_id}",
        f"camera:{camera_model or 'unknown'}",
    ]
    for tag in tags:
        li = ET.SubElement(bag, f"{_RDF}li")
        li.text = tag

    xml_bytes = ET.tostring(xmpmeta, encoding="unicode", xml_declaration=False)

    sidecar = image_path.with_suffix(".xmp")
    with open(sidecar, "w", encoding="utf-8") as f:
        f.write('<?xpacket begin="\xef\xbb\xbf" id="W5M0MpCehiHzreSzNTczkc9d"?>\n')
        f.write(xml_bytes)
        f.write('\n<?xpacket end="w"?>\n')

    return sidecar
