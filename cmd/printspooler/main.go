package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	_ "image/jpeg"
	_ "image/png"

	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/gobold"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"

	qrcode "github.com/skip2/go-qrcode"
)

type Config struct {
	BackendURL       string
	LoginUser        string
	LoginPassword    string
	ImpresoraID      int
	PollInterval     time.Duration
	FetchLimit       int
	PrintBackend     string
	PrinterName      string
	PrintCommand     string
	OutputDir        string
	LogJSONPayload   bool
	RequestTimeout   time.Duration
	RenderMode       string
	LogoImagePath    string
}

type LoginResponse struct {
	AccessToken string `json:"accessToken"`
	Access      string `json:"access"`
	Token       string `json:"token"`
}

type PendingResponse struct {
	Success bool      `json:"success"`
	Data    []PoolJob `json:"data"`
}

type PoolJob struct {
	ID            int64           `json:"id"`
	FolioPaquete  *string         `json:"folio_paquete"`
	ImpresoraID   *int64          `json:"impresora_id"`
	PedidoID      *int64          `json:"pedidoId"`
	Mesa          *string         `json:"mesa"`
	Origen        string          `json:"origen"`
	Estado        string          `json:"estado"`
	CreatedByName *string         `json:"createdByNombre"`
	EmpresaNombre *string         `json:"empresaNombre"`
	CreatedAt     string          `json:"createdAt"`
	Payload       json.RawMessage `json:"payload"`
}

type TicketDoc struct {
	Text string
	ZPL  []byte
}

type APIClient struct {
	cfg    Config
	http   *http.Client
	token  string
}

func main() {
	if err := run(); err != nil {
		log.Fatalf("fatal: %v", err)
	}
}

func run() error {
	loadDotEnv(".env")
	cfg, err := loadConfig()
	if err != nil {
		return err
	}

	if cfg.PrintBackend == "file" {
		if err := os.MkdirAll(cfg.OutputDir, 0o755); err != nil {
			return fmt.Errorf("no se pudo crear OUTPUT_DIR: %w", err)
		}
	}

	client := &APIClient{
		cfg: cfg,
		http: &http.Client{Timeout: cfg.RequestTimeout},
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	log.Printf("print-spooler iniciado | backend=%s | intervalo=%s | print_backend=%s", cfg.BackendURL, cfg.PollInterval, cfg.PrintBackend)

	ticker := time.NewTicker(cfg.PollInterval)
	defer ticker.Stop()

	for {
		if err := cycle(ctx, client); err != nil {
			log.Printf("error en ciclo: %v", err)
		}

		select {
		case <-ctx.Done():
			log.Println("apagando print-spooler")
			return nil
		case <-ticker.C:
		}
	}
}

func cycle(ctx context.Context, client *APIClient) error {
	if err := client.ensureToken(ctx); err != nil {
		return fmt.Errorf("login fallido: %w", err)
	}

	jobs, err := client.fetchPending(ctx)
	if err != nil {
		return fmt.Errorf("no se pudo obtener cola: %w", err)
	}

	if len(jobs) == 0 {
		return nil
	}

	log.Printf("pendientes recibidos: %d", len(jobs))

	for _, job := range jobs {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		doc := renderEtiquetaJob(client.cfg, job)
		if client.cfg.LogJSONPayload {
			log.Printf("job=%d payload=%s", job.ID, string(job.Payload))
		}

		if err := printTicket(client.cfg, job.ID, doc); err != nil {
			log.Printf("job=%d error imprimiendo: %v", job.ID, err)
			if markErr := client.markError(ctx, job.ID); markErr != nil {
				log.Printf("job=%d no se pudo marcar ERROR: %v", job.ID, markErr)
			}
			continue
		}

		if err := client.markPrinted(ctx, job.ID); err != nil {
			log.Printf("job=%d no se pudo marcar IMPRESO: %v", job.ID, err)
			continue
		}

		log.Printf("job=%d impreso y confirmado", job.ID)
	}

	return nil
}

func loadConfig() (Config, error) {
	cfg := Config{
		BackendURL:     normalizeBackendURL(strings.TrimSpace(os.Getenv("BACKEND_URL"))),
		LoginUser:      strings.TrimSpace(os.Getenv("PRINT_USER")),
		LoginPassword:  strings.TrimSpace(os.Getenv("PRINT_PASSWORD")),
		ImpresoraID:    parseIntOrDefault(os.Getenv("IMPRESORA_ID"), 0),
		PrintBackend:   defaultString(strings.TrimSpace(os.Getenv("PRINT_BACKEND")), "file"),
		PrinterName:    strings.TrimSpace(os.Getenv("PRINTER_NAME")),
		PrintCommand:   strings.TrimSpace(os.Getenv("PRINT_COMMAND")),
		OutputDir:      defaultString(strings.TrimSpace(os.Getenv("OUTPUT_DIR")), "./outbox"),
		LogJSONPayload: strings.EqualFold(strings.TrimSpace(os.Getenv("LOG_JSON_PAYLOAD")), "true"),
		RenderMode:     defaultString(strings.TrimSpace(os.Getenv("RENDER_MODE")), "zpl"),
		LogoImagePath:  defaultString(strings.TrimSpace(os.Getenv("LOGO_IMAGE_PATH")), "./cmd/printspooler/logo.png"),
	}

	if cfg.BackendURL == "" {
		return cfg, errors.New("BACKEND_URL es requerido")
	}
	if cfg.LoginUser == "" || cfg.LoginPassword == "" {
		return cfg, errors.New("PRINT_USER y PRINT_PASSWORD son requeridos")
	}
	if cfg.ImpresoraID <= 0 {
		return cfg, errors.New("IMPRESORA_ID es requerido y debe ser mayor a 0")
	}

	pollSeconds := parseIntOrDefault(os.Getenv("POLL_INTERVAL_SECONDS"), 4)
	if pollSeconds < 1 {
		pollSeconds = 1
	}
	cfg.PollInterval = time.Duration(pollSeconds) * time.Second

	cfg.FetchLimit = parseIntOrDefault(os.Getenv("FETCH_LIMIT"), 25)
	if cfg.FetchLimit < 1 {
		cfg.FetchLimit = 1
	}
	if cfg.FetchLimit > 200 {
		cfg.FetchLimit = 200
	}

	timeoutSeconds := parseIntOrDefault(os.Getenv("REQUEST_TIMEOUT_SECONDS"), 20)
	if timeoutSeconds < 5 {
		timeoutSeconds = 5
	}
	cfg.RequestTimeout = time.Duration(timeoutSeconds) * time.Second

	switch strings.ToLower(cfg.PrintBackend) {
	case "lp", "command", "file":
	default:
		return cfg, errors.New("PRINT_BACKEND debe ser lp, command o file")
	}

	if strings.EqualFold(cfg.PrintBackend, "command") && cfg.PrintCommand == "" {
		return cfg, errors.New("PRINT_COMMAND es requerido cuando PRINT_BACKEND=command")
	}

	return cfg, nil
}

func normalizeBackendURL(raw string) string {
	base := strings.TrimSpace(raw)
	base = strings.TrimRight(base, "/")
	if strings.HasSuffix(strings.ToLower(base), "/api") {
		base = strings.TrimSuffix(base, "/api")
		base = strings.TrimSuffix(base, "/API")
		base = strings.TrimRight(base, "/")
	}
	return base
}

func (c *APIClient) ensureToken(ctx context.Context) error {
	if c.token != "" {
		return nil
	}
	return c.login(ctx)
}

func (c *APIClient) login(ctx context.Context) error {
	payload := map[string]string{
		"username":    c.cfg.LoginUser,
		"password": c.cfg.LoginPassword,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.cfg.BackendURL, "/")+"/api/login/", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("login status=%d body=%s", resp.StatusCode, string(raw))
	}

	var parsed LoginResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return err
	}
	token := strings.TrimSpace(parsed.AccessToken)
	if token == "" {
		token = strings.TrimSpace(parsed.Access)
	}
	if token == "" {
		token = strings.TrimSpace(parsed.Token)
	}
	if token == "" {
		return errors.New("login sin accessToken")
	}

	c.token = token
	return nil
}

func (c *APIClient) fetchPending(ctx context.Context) ([]PoolJob, error) {
	v := url.Values{}
	v.Set("limit", strconv.Itoa(c.cfg.FetchLimit))
	v.Set("impresora_id", strconv.Itoa(c.cfg.ImpresoraID))
	endpoint := strings.TrimRight(c.cfg.BackendURL, "/") + "/api/etiquetas-impresion/pool/pending/?" + v.Encode()

	resp, err := c.doAuthRequest(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("pending status=%d body=%s", resp.StatusCode, string(raw))
	}

	var parsed PendingResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, err
	}

	return parsed.Data, nil
}

func (c *APIClient) markPrinted(ctx context.Context, id int64) error {
	endpoint := fmt.Sprintf("%s/api/etiquetas-impresion/pool/%d/printed/", strings.TrimRight(c.cfg.BackendURL, "/"), id)
	return c.simplePost(ctx, endpoint)
}

func (c *APIClient) markError(ctx context.Context, id int64) error {
	endpoint := fmt.Sprintf("%s/api/etiquetas-impresion/pool/%d/error/", strings.TrimRight(c.cfg.BackendURL, "/"), id)
	return c.simplePost(ctx, endpoint)
}

func (c *APIClient) simplePost(ctx context.Context, endpoint string) error {
	resp, err := c.doAuthRequest(ctx, http.MethodPost, endpoint, []byte("{}"))
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("post status=%d body=%s", resp.StatusCode, string(raw))
	}

	return nil
}

func (c *APIClient) doAuthRequest(ctx context.Context, method, endpoint string, body []byte) (*http.Response, error) {
	if err := c.ensureToken(ctx); err != nil {
		return nil, err
	}

	mkReq := func(tok string) (*http.Request, error) {
		var reader io.Reader
		if len(body) > 0 {
			reader = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Content-Type", "application/json")
		return req, nil
	}

	req, err := mkReq(c.token)
	if err != nil {
		return nil, err
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusUnauthorized {
		return resp, nil
	}

	resp.Body.Close()
	c.token = ""
	if err := c.login(ctx); err != nil {
		return nil, err
	}

	req2, err := mkReq(c.token)
	if err != nil {
		return nil, err
	}

	return c.http.Do(req2)
}

func printTicket(cfg Config, jobID int64, doc TicketDoc) error {
	// Solo etiquetas de envío: ZPL o texto
	switch strings.ToLower(cfg.PrintBackend) {
	case "lp":
		args := []string{}
		if strings.TrimSpace(cfg.PrinterName) != "" {
			args = append(args, "-d", cfg.PrinterName)
		}
		args = append(args, "-")
		cmd := exec.Command("lp", args...)
		if strings.ToLower(cfg.RenderMode) == "zpl" && len(doc.ZPL) > 0 {
			cmd.Stdin = bytes.NewReader(doc.ZPL)
		} else {
			cmd.Stdin = strings.NewReader(doc.Text)
		}
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("lp error: %w output=%s", err, string(out))
		}
		return nil
	case "command":
		var cmd *exec.Cmd
		if runtime.GOOS == "windows" {
			cmd = exec.Command("powershell", "-NoProfile", "-Command", cfg.PrintCommand)
		} else {
			cmd = exec.Command("sh", "-c", cfg.PrintCommand)
		}
		if strings.ToLower(cfg.RenderMode) == "zpl" && len(doc.ZPL) > 0 {
			cmd.Stdin = bytes.NewReader(doc.ZPL)
		} else {
			cmd.Stdin = strings.NewReader(doc.Text)
		}
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("command error: %w output=%s", err, string(out))
		}
		return nil
	default:
		ext := "zpl"
		data := doc.ZPL
		if strings.ToLower(cfg.RenderMode) == "text" || len(data) == 0 {
			ext = "txt"
			data = []byte(doc.Text)
		}
		fileName := filepath.Join(cfg.OutputDir, fmt.Sprintf("job_%d_%d.%s", jobID, time.Now().UnixNano(), ext))
		if err := os.WriteFile(fileName, data, 0o644); err != nil {
			return fmt.Errorf("file print error: %w", err)
		}
		return nil
	}
}

func renderEtiquetaJob(cfg Config, job PoolJob) TicketDoc {
	var payload map[string]any
	_ = json.Unmarshal(job.Payload, &payload)

	folio := strings.TrimSpace(stringFromAny(payload["folio_paquete"]))
	if folio == "" && job.FolioPaquete != nil {
		folio = strings.TrimSpace(*job.FolioPaquete)
	}
	if folio == "" {
		folio = fmt.Sprintf("JOB-%d", job.ID)
	}

	var b strings.Builder
	b.WriteString("========================================\n")
	b.WriteString("       ETIQUETA DE ENVIO (ZPL)          \n")
	b.WriteString("========================================\n")
	b.WriteString(fmt.Sprintf("Trabajo: #%d\n", job.ID))
	b.WriteString(fmt.Sprintf("Folio:   %s\n", folio))
	b.WriteString(fmt.Sprintf("Ruta:    %s\n", stringFromAny(payload["ruta_paquete"])))
	b.WriteString("========================================\n")

	return TicketDoc{
		Text: b.String(),
		ZPL:  renderEtiquetaEnvioZPL(cfg, payload),
	}
}

func renderEtiquetaEnvioZPL(cfg Config, payload map[string]any) []byte {
	folio := stringOrDefault(stringFromAny(payload["folio_paquete"]), " -- ")
	cliente := stringOrDefault(stringFromAny(payload["cliente_nombre"]), " -- ")
	estado := stringOrDefault(stringFromAny(payload["estado_nombre"]), " -- ")
	municipio := stringOrDefault(stringFromAny(payload["municipio_nombre"]), " -- ")
	ruta := stringOrDefault(stringFromAny(payload["ruta_paquete"]), " Ruta por definir ")
	tipoProducto := stringOrDefault(stringFromAny(payload["tipo_producto_text"]), " -- ")
	peso := strings.TrimSpace(stringFromAny(payload["peso"]))
	alto := strings.TrimSpace(stringFromAny(payload["alto"]))
	largo := strings.TrimSpace(stringFromAny(payload["largo"]))
	ancho := strings.TrimSpace(stringFromAny(payload["ancho"]))
	esDomicilio := boolFromAny(payload["es_domicilio"])
	esPieza := boolFromAny(payload["es_pieza"])
	sobrepeso := boolFromAny(payload["sobrepeso"])

	destino := strings.TrimSpace(stringFromAny(payload["domicilio_completo"]))
	if !esDomicilio {
		destino = strings.TrimSpace(stringFromAny(payload["sucursal_nombre"]))
	}
	if destino == "" {
		destino = " -- "
	}

	var b strings.Builder
	// 4x6 thermal label @203dpi => ~812 x 1218 dots; contenido bien ensanchado en X
	labelWidth := 812
	labelHeight := 1218
	leftMargin := 15 // margen mínimo para ensanchar al máximo el contenido
	y := 20
	w := func(s string) { b.WriteString(s) }
	centerLine := func(text string, fontH, fontW int) {
		w(fmt.Sprintf("^FO0,%d^A0N,%d,%d^FB%d,1,0,C,0^FD%s^FS", y, fontH, fontW, labelWidth, zplSafe(text)))
		y += fontH + 8
	}
	// Menos espacio entre líneas (para bloque tipo producto / peso / medidas)
	centerLineCompact := func(text string, fontH, fontW int) {
		w(fmt.Sprintf("^FO0,%d^A0N,%d,%d^FB%d,1,0,C,0^FD%s^FS", y, fontH, fontW, labelWidth, zplSafe(text)))
		y += fontH + 4
	}
	leftLine := func(text string, fontH, fontW int) {
		w(fmt.Sprintf("^FO%d,%d^A0N,%d,%d^FD%s^FS", leftMargin, y, fontH, fontW, zplSafe(text)))
		y += fontH + 8
	}
	// Negrita mismo tamaño que leftLine: dos pasadas con 1 dot de desplazamiento
	leftLineBold := func(text string, fontH, fontW int) {
		w(fmt.Sprintf("^FO%d,%d^A0N,%d,%d^FD%s^FS", leftMargin, y, fontH, fontW, zplSafe(text)))
		w(fmt.Sprintf("^FO%d,%d^A0N,%d,%d^FD%s^FS", leftMargin+1, y, fontH, fontW, zplSafe(text)))
		y += fontH + 8
	}

	w("^XA^CI28^PW812") // ^CI28 = UTF-8 para que salgan bien acentos (é, á, ñ, etc.)
	logoWidth := 420
	logoHeight := 200 // alargado en Y (antes 120)
	logoX := (labelWidth - logoWidth) / 2
	logoY := 20
	logoCmd := buildLogoGFACommand(cfg.LogoImagePath, logoX, logoY, logoWidth, logoHeight)
	if logoCmd != "" {
		w(logoCmd)
	} else {
		y = 55
		centerLine("PUEBLA EXPRESS", 48, 26)
	}

	// Logo → folio → QR (subido en Y); luego gap → tipo producto → peso → medidas → Cliente
	y = logoY + logoHeight + 0 // sin hueco para subir folio y QR
	centerLine(folio, 72, 50)
	y += 0 // QR pegado al folio para subirlo más
	qrY := y
	qrSize := 380
	qrX := (labelWidth - qrSize) / 2
	qrGFA := buildQRGFA(folio, qrX, qrY, qrSize)
	if qrGFA != "" {
		w(qrGFA)
	} else {
		w(fmt.Sprintf("^FO%d,%d^BQN,2,10^FDLA,%s^FS", qrX, qrY, zplSafe(folio)))
	}
	// PDF: después del QR hace y += 120 (QR 105mm + 15mm gap). En 4x6" ~24 dots de gap.
	y = qrY + qrSize + 24
	// Tipo de producto debajo del QR; padding top generoso para que no se corten las letras por arriba
	if cmd := buildBoldTextGFA(tipoProducto, 0, y, labelWidth, 50, true, 20); cmd != "" {
		w(cmd)
		y += 70 + 6 // 50+20 altura GFA + espacio
	} else {
		w(fmt.Sprintf("^FO0,%d^A0N,48,30^FB%d,1,0,C,0^FD%s^FS", y, labelWidth, zplSafe(tipoProducto)))
		y += 48 + 6
	}
	// Peso (PDF: mismo tamaño que tipo, luego y += 10)
	if !esPieza && peso != "" {
		centerLineCompact(peso, 40, 22)
	}
	// Medidas (PDF: font 20, y += 10)
	if alto != "" || largo != "" || ancho != "" {
		centerLineCompact(fmt.Sprintf(`%s" x %s" x %s"`, alto, largo, ancho), 34, 24)
	}
	y += 16 // PDF: espacio antes del bloque Cliente (padding top)
	leftLine("Cliente: "+cliente, 50, 32)
	if esDomicilio {
		leftLine("Domicilio: "+destino, 46, 28)
	} else {
		leftLine("Sucursal: "+destino, 46, 28)
	}
	leftLine("Estado: "+estado, 46, 28)
	leftLine("Municipio: "+municipio, 46, 28)
	leftLine("Bodega descarga: PUE", 46, 28)
	// Ruta mismo tamaño que Cliente (50, 32) pero en negrita
	leftLineBold("Ruta: "+ruta, 50, 32)
	if sobrepeso {
		leftLine("Sobrepeso", 58, 36)
	}

	labelLen := y + 40
	if labelLen > labelHeight {
		labelLen = labelHeight
	}
	w("^XZ")
	return []byte(strings.Replace(b.String(), "^XA^CI28^PW812", fmt.Sprintf("^XA^CI28^PW812^LL%d^MNY", labelLen), 1))
}

func buildLogoGFACommand(path string, x, y, targetW, targetH int) string {
	p := resolveLogoPath(strings.TrimSpace(path))
	if p == "" {
		return ""
	}
	f, err := os.Open(p)
	if err != nil {
		return ""
	}
	defer f.Close()

	img, _, err := image.Decode(f)
	if err != nil {
		return ""
	}

	srcB := img.Bounds()
	srcW := srcB.Dx()
	srcH := srcB.Dy()
	if srcW <= 0 || srcH <= 0 || targetW <= 0 || targetH <= 0 {
		return ""
	}

	bytesPerRow := (targetW + 7) / 8
	data := make([]byte, bytesPerRow*targetH)
	for yy := 0; yy < targetH; yy++ {
		sy := srcB.Min.Y + (yy*srcH)/targetH
		for xx := 0; xx < targetW; xx++ {
			sx := srcB.Min.X + (xx*srcW)/targetW
			r, g, bb, a := img.At(sx, sy).RGBA()
			alpha := uint8(a >> 8)
			if alpha < 120 {
				continue
			}
			r8 := float64(uint8(r >> 8))
			g8 := float64(uint8(g >> 8))
			b8 := float64(uint8(bb >> 8))
			luma := 0.299*r8 + 0.587*g8 + 0.114*b8
			if luma < 160 {
				idx := yy*bytesPerRow + (xx / 8)
				data[idx] |= 1 << uint(7-(xx%8))
			}
		}
	}

	total := len(data)
	hexData := strings.ToUpper(hex.EncodeToString(data))
	return fmt.Sprintf("^FO%d,%d^GFA,%d,%d,%d,%s^FS", x, y, total, total, bytesPerRow, hexData)
}

// buildQRGFA genera un QR del contenido como gráfico ^GFA para fijar tamaño (p. ej. igual que etiqueta original).
func buildQRGFA(content string, x, y, size int) string {
	if size <= 0 || content == "" {
		return ""
	}
	pngBytes, err := qrcode.Encode(content, qrcode.Medium, size)
	if err != nil {
		return ""
	}
	img, _, err := image.Decode(bytes.NewReader(pngBytes))
	if err != nil {
		return ""
	}
	bounds := img.Bounds()
	srcW := bounds.Dx()
	srcH := bounds.Dy()
	if srcW <= 0 || srcH <= 0 {
		return ""
	}
	bytesPerRow := (size + 7) / 8
	data := make([]byte, bytesPerRow*size)
	for yy := 0; yy < size; yy++ {
		sy := bounds.Min.Y + (yy*srcH)/size
		for xx := 0; xx < size; xx++ {
			sx := bounds.Min.X + (xx*srcW)/size
			r, g, bb, a := img.At(sx, sy).RGBA()
			if a>>8 < 120 {
				continue
			}
			r8 := float64(uint8(r >> 8))
			g8 := float64(uint8(g >> 8))
			b8 := float64(uint8(bb >> 8))
			luma := 0.299*r8 + 0.587*g8 + 0.114*b8
			if luma < 160 {
				idx := yy*bytesPerRow + (xx / 8)
				data[idx] |= 1 << uint(7-(xx%8))
			}
		}
	}
	total := len(data)
	hexData := strings.ToUpper(hex.EncodeToString(data))
	return fmt.Sprintf("^FO%d,%d^GFA,%d,%d,%d,%s^FS", x, y, total, total, bytesPerRow, hexData)
}

// buildBoldTextGFA dibuja el texto con fuente bold (Go Bold TTF) y lo devuelve como ^GFA para ZPL.
// Si el texto es más ancho que width, se reduce el tamaño de fuente para que quepa.
// topPadding añade píxeles en blanco arriba del texto para que no se corten ascendentes (ej. en "Libras").
func buildBoldTextGFA(text string, posX, posY, width, height int, center bool, topPadding int) string {
	if text == "" || width <= 0 || height <= 0 {
		return ""
	}
	if topPadding < 0 {
		topPadding = 0
	}
	ttf, err := opentype.Parse(gobold.TTF)
	if err != nil {
		return ""
	}
	// Tamaño en puntos: limitado por altura para que no se corte
	pt := float64(height) * 0.65
	if pt < 6 {
		pt = 6
	}
	// Reducir pt hasta que el texto quepa en width (para rutas largas, etc.)
	for pt >= 6 {
		tryFace, err := opentype.NewFace(ttf, &opentype.FaceOptions{Size: pt, DPI: 203})
		if err != nil {
			return ""
		}
		drMeasure := &font.Drawer{Face: tryFace}
		adv := drMeasure.MeasureString(text)
		tryFace.Close()
		if adv.Round() <= width {
			break
		}
		pt *= 0.85
	}
	if pt < 6 {
		pt = 6
	}
	face, err := opentype.NewFace(ttf, &opentype.FaceOptions{Size: pt, DPI: 203})
	if err != nil {
		return ""
	}
	defer face.Close()

	totalHeight := height + topPadding
	img := image.NewRGBA(image.Rect(0, 0, width, totalHeight))
	draw.Draw(img, img.Bounds(), &image.Uniform{color.White}, image.Point{}, draw.Src)

	dr := &font.Drawer{
		Dst:  img,
		Src:  image.NewUniform(color.Black),
		Face: face,
	}
	adv := dr.MeasureString(text)
	advPx := adv.Round()
	xPx := 0
	if center && advPx < width {
		xPx = (width - advPx) / 2
	}
	ascent := face.Metrics().Ascent.Round()
	// Baseline un poco más abajo para dejar margen arriba (evita que se corten L, b, s en "Libras")
	baselineY := topPadding + ascent + 8
	if baselineY+4 > totalHeight {
		baselineY = totalHeight - 4
	}
	dr.Dot = fixed.P(xPx, baselineY)
	dr.DrawString(text)

	// Convertir RGBA a 1-bit (negro = 1) y luego a GFA
	bytesPerRow := (width + 7) / 8
	data := make([]byte, bytesPerRow*totalHeight)
	bounds := img.Bounds()
	for yy := 0; yy < totalHeight; yy++ {
		for xx := 0; xx < width; xx++ {
			r, g, bb, a := img.At(bounds.Min.X+xx, bounds.Min.Y+yy).RGBA()
			if a>>8 < 120 {
				continue
			}
			r8 := float64(uint8(r >> 8))
			g8 := float64(uint8(g >> 8))
			b8 := float64(uint8(bb >> 8))
			luma := 0.299*r8 + 0.587*g8 + 0.114*b8
			if luma < 200 {
				idx := yy*bytesPerRow + (xx / 8)
				data[idx] |= 1 << uint(7-(xx%8))
			}
		}
	}
	total := len(data)
	hexData := strings.ToUpper(hex.EncodeToString(data))
	return fmt.Sprintf("^FO%d,%d^GFA,%d,%d,%d,%s^FS", posX, posY, total, total, bytesPerRow, hexData)
}

func resolveLogoPath(raw string) string {
	candidates := []string{}
	if strings.TrimSpace(raw) != "" {
		candidates = append(candidates, raw)
	}
	candidates = append(candidates,
		"./cmd/printspooler/logo.png",
		"./cmd/printspooler/logo.jpg",
		"./cmd/printspooler/images/logo.png",
		"./assets/logo.png",
		"./images/logo.png",
		"./logo.png",
	)

	if exe, err := os.Executable(); err == nil {
		exeDir := filepath.Dir(exe)
		candidates = append(candidates,
			filepath.Join(exeDir, "logo.png"),
			filepath.Join(exeDir, "images", "logo.png"),
		)
	}

	seen := map[string]bool{}
	for _, c := range candidates {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		if !filepath.IsAbs(c) {
			if abs, err := filepath.Abs(c); err == nil {
				c = abs
			}
		}
		if seen[c] {
			continue
		}
		seen[c] = true
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			return c
		}
	}
	return ""
}

func zplSafe(s string) string {
	s = strings.ReplaceAll(s, "^", "")
	s = strings.ReplaceAll(s, "~", "")
	return s
}

func loadDotEnv(path string) {
	content, err := os.ReadFile(path)
	if err != nil {
		return
	}

	for _, line := range strings.Split(string(content), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		val := strings.TrimSpace(parts[1])
		quoted := false
		if len(val) >= 2 {
			if (val[0] == '\'' && val[len(val)-1] == '\'') || (val[0] == '"' && val[len(val)-1] == '"') {
				val = val[1 : len(val)-1]
				quoted = true
			}
		}
		if !quoted {
			if idx := strings.Index(val, " #"); idx >= 0 {
				val = strings.TrimSpace(val[:idx])
			}
		}
		if _, exists := os.LookupEnv(key); !exists {
			_ = os.Setenv(key, val)
		}
	}
}

func parseIntOrDefault(raw string, def int) int {
	v, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return def
	}
	return v
}

func defaultString(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}

func intOrDefault(v int, def int) int {
	if v <= 0 {
		return def
	}
	return v
}

func stringOrDefault(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}

func stringFromAny(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case fmt.Stringer:
		return x.String()
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case int:
		return strconv.Itoa(x)
	case int64:
		return strconv.FormatInt(x, 10)
	default:
		return ""
	}
}

func intFromAny(v any) int {
	switch x := v.(type) {
	case float64:
		return int(x)
	case int:
		return x
	case int64:
		return int(x)
	case string:
		i, _ := strconv.Atoi(strings.TrimSpace(x))
		return i
	default:
		return 0
	}
}

func boolFromAny(v any) bool {
	switch x := v.(type) {
	case bool:
		return x
	case float64:
		return x != 0
	case int:
		return x != 0
	case int64:
		return x != 0
	case string:
		s := strings.TrimSpace(strings.ToLower(x))
		return s == "1" || s == "true" || s == "yes" || s == "si"
	default:
		return false
	}
}
