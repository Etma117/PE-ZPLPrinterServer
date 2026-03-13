# Envia stdin por TCP a la impresora, o en modo preview renderiza ZPL a imagen y abre
# el archivo para que puedas usar "Imprimir" > "Microsoft Print to PDF".
#
# Modo impresora: define PRINTER_ADDRESS (ej. 192.168.1.70:9100).
# Modo preview/PDF: define USE_PDF_PREVIEW=1 o deja PRINTER_ADDRESS vacio.
import os
import sys
import socket
import urllib.request
import tempfile
import subprocess
import platform

ZPL = sys.stdin.buffer.read()
USE_PDF_PREVIEW = os.environ.get("USE_PDF_PREVIEW", "").strip().lower() in ("1", "true", "yes")
PRINTER_ADDRESS = (os.environ.get("PRINTER_ADDRESS") or "").strip()

if USE_PDF_PREVIEW or not PRINTER_ADDRESS:
    # Renderizar ZPL a imagen con Labelary y abrir para imprimir a PDF
    # 4x6 pulgadas, 8dpmm (~203 dpi)
    url = "http://api.labelary.com/v1/printers/8dpmm/labels/4x6/0/"
    req = urllib.request.Request(url, data=ZPL, method="POST")
    req.add_header("Content-Type", "application/x-www-form-urlencoded")
    try:
        with urllib.request.urlopen(req, timeout=15) as resp:
            png_data = resp.read()
    except Exception as e:
        sys.stderr.write(f"Error al renderizar ZPL: {e}\n")
        sys.exit(1)

    out_dir = os.environ.get("OUTPUT_DIR", tempfile.gettempdir())
    os.makedirs(out_dir, exist_ok=True)
    out_path = os.path.join(out_dir, "etiqueta_zpl_preview.png")
    with open(out_path, "wb") as f:
        f.write(png_data)

    if platform.system() == "Windows":
        os.startfile(out_path)
    elif platform.system() == "Darwin":
        subprocess.run(["open", out_path], check=False)
    else:
        subprocess.run(["xdg-open", out_path], check=False)
    sys.stderr.write(f"Preview guardado: {out_path}. Usa Imprimir > Microsoft Print to PDF.\n")
else:
    host, port = PRINTER_ADDRESS.rsplit(":", 1)
    port = int(port)
    s = socket.create_connection((host, port), timeout=10)
    s.sendall(ZPL)
    s.close()
