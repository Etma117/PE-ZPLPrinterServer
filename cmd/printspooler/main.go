package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
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

type TicketExtra struct {
	Nombre   string
	Cantidad int
	Precio   float64
}

type TicketItem struct {
	Nombre      string
	Cantidad    int
	Subtotal    float64
	Observacion string
	Extras      []TicketExtra
}

type TicketDoc struct {
	HTML   string
	Text   string
	ESCPos []byte
	ZPL    []byte
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
	switch strings.ToLower(cfg.PrintBackend) {
	case "lp":
		args := []string{}
		if strings.TrimSpace(cfg.PrinterName) != "" {
			args = append(args, "-d", cfg.PrinterName)
		}
		args = append(args, "-")
		cmd := exec.Command("lp", args...)
		cmd.Stdin = strings.NewReader(doc.Text)
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
		switch strings.ToLower(cfg.RenderMode) {
		case "escpos":
			cmd.Stdin = bytes.NewReader(doc.ESCPos)
		case "zpl":
			cmd.Stdin = bytes.NewReader(doc.ZPL)
		case "text":
			cmd.Stdin = strings.NewReader(doc.Text)
		default:
			cmd.Stdin = strings.NewReader(doc.HTML)
		}
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("command error: %w output=%s", err, string(out))
		}
		return nil
	default:
		var data []byte
		var ext string
		switch strings.ToLower(cfg.RenderMode) {
		case "escpos":
			data = doc.ESCPos
			ext = "bin"
		case "zpl":
			data = doc.ZPL
			ext = "zpl"
		case "text":
			data = []byte(doc.Text)
			ext = "txt"
		default:
			data = []byte(doc.HTML)
			ext = "html"
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
	// 4x6 thermal label @203dpi => ~812 x 1218 dots
	labelWidth := 812
	labelHeight := 1218
	y := 20
	w := func(s string) { b.WriteString(s) }
	centerLine := func(text string, fontH, fontW int) {
		w(fmt.Sprintf("^FO0,%d^A0N,%d,%d^FB%d,1,0,C,0^FD%s^FS", y, fontH, fontW, labelWidth, zplSafe(text)))
		y += fontH + 8
	}
	leftLine := func(text string, fontH, fontW int) {
		w(fmt.Sprintf("^FO90,%d^A0N,%d,%d^FD%s^FS", y, fontH, fontW, zplSafe(text)))
		y += fontH + 8
	}

	w("^XA^PW812")
	logoWidth := 420
	logoHeight := 120
	logoX := (labelWidth - logoWidth) / 2
	logoY := 20
	logoCmd := buildLogoGFACommand(cfg.LogoImagePath, logoX, logoY, logoWidth, logoHeight)
	if logoCmd != "" {
		w(logoCmd)
	} else {
		y = 55
		centerLine("PUEBLA EXPRESS", 48, 26)
	}

	y = logoY + logoHeight + 48
	centerLine(folio, 72, 38)
	y += 14
	// QR como ^GFA para controlar tamaño (igual que etiqueta original: ~105/220 del ancho → ~387 dots; usamos 380)
	qrSize := 380
	qrX := (labelWidth - qrSize) / 2
	qrGFA := buildQRGFA(folio, qrX, y, qrSize)
	if qrGFA != "" {
		w(qrGFA)
	} else {
		w(fmt.Sprintf("^FO%d,%d^BQN,2,10^FDLA,%s^FS", qrX, y, zplSafe(folio)))
	}
	y += qrSize
	centerLine(tipoProducto, 68, 36)
	if !esPieza && peso != "" {
		centerLine(peso, 68, 36)
	}
	if alto != "" || largo != "" || ancho != "" {
		centerLine(fmt.Sprintf(`%s" x %s" x %s"`, alto, largo, ancho), 34, 18)
	}
	y += 8
	leftLine("Cliente: "+cliente, 50, 26)
	if esDomicilio {
		leftLine("Domicilio: "+destino, 46, 24)
	} else {
		leftLine("Sucursal: "+destino, 46, 24)
	}
	leftLine("Estado: "+estado, 46, 24)
	leftLine("Municipio: "+municipio, 46, 24)
	leftLine("Bodega descarga: PUE", 46, 24)
	leftLine("Ruta: "+ruta, 58, 30)
	if sobrepeso {
		leftLine("Sobrepeso", 58, 30)
	}

	labelLen := y + 40
	if labelLen > labelHeight {
		labelLen = labelHeight
	}
	w("^XZ")
	return []byte(strings.Replace(b.String(), "^XA^PW812", fmt.Sprintf("^XA^PW812^LL%d^MNY", labelLen), 1))
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

func renderTicket(job PoolJob) TicketDoc {
	var payload map[string]any
	_ = json.Unmarshal(job.Payload, &payload)

	pedidoID := "-"
	if job.PedidoID != nil {
		pedidoID = strconv.FormatInt(*job.PedidoID, 10)
	}

	mesa := "-"
	if job.Mesa != nil {
		mesa = *job.Mesa
	}

	total := numberFromAny(payload["total"])
	items := parseTicketItems(payload["detalles"])
	nombreComercial := stringFromAny(payload["nombreComercial"])
	if strings.TrimSpace(nombreComercial) == "" && job.EmpresaNombre != nil {
		nombreComercial = *job.EmpresaNombre
	}
	nombreComercial = stringOrDefault(nombreComercial, "EMPRESA")
	direccion := stringFromAny(payload["direccion"])
	telefono := stringFromAny(payload["telefono"])
	mensaje := stringFromAny(payload["mensaje"])
	leyenda := stringFromAny(payload["leyendaFiscal"])
	solicitadoPor := ""
	if job.CreatedByName != nil {
		solicitadoPor = strings.TrimSpace(*job.CreatedByName)
	}
	if solicitadoPor == "" {
		solicitadoPor = strings.TrimSpace(stringFromAny(payload["atendio"]))
	}
	if solicitadoPor == "" {
		solicitadoPor = strings.TrimSpace(stringFromAny(payload["usuarioNombre"]))
	}

	var b strings.Builder
	b.WriteString("========================================\n")
	b.WriteString("           TICKET DE IMPRESION          \n")
	b.WriteString("========================================\n")
	b.WriteString(fmt.Sprintf("Trabajo: #%d\n", job.ID))
	b.WriteString(fmt.Sprintf("Pedido:  #%s\n", pedidoID))
	b.WriteString(fmt.Sprintf("Mesa:    %s\n", mesa))
	b.WriteString(fmt.Sprintf("Origen:  %s\n", job.Origen))
	if solicitadoPor != "" {
		b.WriteString(fmt.Sprintf("Solicito: %s\n", solicitadoPor))
	}
	b.WriteString(fmt.Sprintf("Fecha:   %s\n", time.Now().Format("2006-01-02 15:04:05")))
	b.WriteString("----------------------------------------\n")
	b.WriteString("Items\n")

	if len(items) == 0 {
		b.WriteString("(sin detalles en payload)\n")
	}

	for _, item := range items {
		b.WriteString(fmt.Sprintf("- %dx %s ..... $%.2f\n", item.Cantidad, item.Nombre, item.Subtotal))
		for _, ex := range item.Extras {
			b.WriteString(fmt.Sprintf("  + %dx %s ..... $%.2f\n", ex.Cantidad, ex.Nombre, ex.Precio))
		}
		if strings.TrimSpace(item.Observacion) != "" {
			b.WriteString(fmt.Sprintf("  Obs: %s\n", item.Observacion))
		}
	}

	b.WriteString("----------------------------------------\n")
	if total > 0 {
		b.WriteString(fmt.Sprintf("TOTAL: $%.2f\n", total))
	}
	b.WriteString("========================================\n")
	html := renderTicketHTML(nombreComercial, direccion, telefono, pedidoID, mesa, solicitadoPor, total, items, mensaje, leyenda)
	escpos := renderTicketESCPOS(nombreComercial, direccion, telefono, pedidoID, mesa, solicitadoPor, total, items, mensaje, leyenda, pedidoID)
	zpl := renderTicketZPL(nombreComercial, direccion, telefono, pedidoID, mesa, solicitadoPor, total, items, mensaje, leyenda, pedidoID)
	return TicketDoc{HTML: html, Text: b.String(), ESCPos: escpos, ZPL: zpl}
}

func parseTicketItems(raw any) []TicketItem {
	detalles, ok := raw.([]any)
	if !ok {
		return nil
	}
	items := make([]TicketItem, 0, len(detalles))
	for _, rawItem := range detalles {
		itemMap, ok := rawItem.(map[string]any)
		if !ok {
			continue
		}
		item := TicketItem{
			Nombre:      stringOrDefault(stringFromAny(itemMap["nombre"]), "Platillo"),
			Cantidad:    intOrDefault(intFromAny(itemMap["cantidad"]), 1),
			Subtotal:    numberFromAny(itemMap["subtotal"]),
			Observacion: stringFromAny(itemMap["observacion"]),
		}

		extrasRaw, _ := itemMap["extras"].([]any)
		for _, rawExtra := range extrasRaw {
			exMap, ok := rawExtra.(map[string]any)
			if !ok {
				continue
			}
			cant := intOrDefault(intFromAny(exMap["cantidad"]), 1)
			sub := numberFromAny(exMap["subtotal"])
			if sub <= 0 {
				sub = numberFromAny(exMap["precioAdicional"]) * float64(cant)
			}
			if sub <= 0 {
				continue
			}
			item.Extras = append(item.Extras, TicketExtra{
				Nombre:   stringOrDefault(stringFromAny(exMap["nombre"]), "Extra"),
				Cantidad: cant,
				Precio:   sub,
			})
		}

		items = append(items, item)
	}
	return items
}

func renderTicketHTML(nombreComercial, direccion, telefono, pedidoID, mesa, solicitadoPor string, total float64, items []TicketItem, mensaje, leyenda string) string {
	var detalles strings.Builder
	for _, item := range items {
		detalles.WriteString(`<div class="item-row"><div class="item-name">` + escapeHTML(item.Nombre) + `</div><div class="item-qty">` + strconv.Itoa(item.Cantidad) + `</div><div class="item-price">$` + fmt.Sprintf("%.2f", item.Subtotal) + `</div></div>`)
		if len(item.Extras) > 0 {
			detalles.WriteString(`<div class="extras">`)
			for _, ex := range item.Extras {
				detalles.WriteString(`<div class="extra-row"><span>` + strconv.Itoa(ex.Cantidad) + ` x ` + escapeHTML(ex.Nombre) + `</span><span>$` + fmt.Sprintf("%.2f", ex.Precio) + `</span></div>`)
			}
			detalles.WriteString(`</div>`)
		}
		if strings.TrimSpace(item.Observacion) != "" {
			detalles.WriteString(`<div class="small" style="margin-left: 10px;">Obs: ` + escapeHTML(item.Observacion) + `</div>`)
		}
	}

	parts := []string{
		`<!DOCTYPE html><html><head><meta charset="UTF-8"><meta name="viewport" content="width=device-width, initial-scale=1.0">`,
		`<title>Ticket #` + escapeHTML(pedidoID) + `</title>`,
		`<style>@page{size:80mm auto;margin:0}*{margin:0;padding:0;box-sizing:border-box}body{font-family:'Courier New',Courier,monospace;width:80mm;max-width:80mm;margin:0;padding:5mm;font-size:12px;line-height:1.4;background:white}.center{text-align:center}.bold{font-weight:bold}.separator{border-top:1px dashed #000;margin:8px 0}.item-row{display:flex;justify-content:space-between;margin:3px 0}.item-name{flex:1;word-wrap:break-word}.item-qty{width:30px;text-align:center}.item-price{width:60px;text-align:right}.extras{margin-left:6px;font-size:11px}.extra-row{display:flex;justify-content:space-between}.total-row{display:flex;justify-content:space-between;font-weight:bold;font-size:14px;margin-top:10px}.footer{margin-top:15px;font-size:10px;text-align:center}.small{font-size:10px}</style></head><body class="ticket-print"><div class="ticket-content">`,
		`<div class="center bold">` + escapeHTML(nombreComercial) + `</div>`,
	}
	if strings.TrimSpace(direccion) != "" {
		parts = append(parts, `<div class="center small">`+escapeHTML(direccion)+`</div>`)
	}
	if strings.TrimSpace(telefono) != "" {
		parts = append(parts, `<div class="center small">Tel: `+escapeHTML(telefono)+`</div>`)
	}
	parts = append(parts,
		`<div class="separator"></div>`,
		`<div class="center bold">TICKET DE VENTA</div>`,
		`<div class="center">Pedido: #`+escapeHTML(pedidoID)+`</div>`,
		`<div class="center small">Mesa: `+escapeHTML(mesa)+`</div>`,
	)
	if strings.TrimSpace(solicitadoPor) != "" {
		parts = append(parts, `<div class="center small">Solicito: `+escapeHTML(solicitadoPor)+`</div>`)
	}
	parts = append(parts,
		`<div class="center small">Fecha: `+time.Now().Format("2006-01-02 15:04:05")+`</div>`,
		`<div class="separator"></div>`,
		`<div class="item-row bold"><div class="item-name">DESCRIPCION</div><div class="item-qty">CANT</div><div class="item-price">PRECIO</div></div>`,
		detalles.String(),
		`<div class="separator"></div>`,
		`<div class="total-row"><div>TOTAL:</div><div>$`+fmt.Sprintf("%.2f", total)+`</div></div>`,
	)
	if strings.TrimSpace(mensaje) != "" {
		parts = append(parts, `<div class="separator"></div><div class="center small">`+escapeHTML(mensaje)+`</div>`)
	}
	if strings.TrimSpace(leyenda) != "" {
		parts = append(parts, `<div class="separator"></div><div class="footer">`+escapeHTML(leyenda)+`</div>`)
	}
	parts = append(parts, `<div class="center small" style="margin-top:15px;">GRACIAS POR SU PREFERENCIA!</div></div></body></html>`)
	return strings.Join(parts, "")
}

// ── ESC/POS renderer ───────────────────────────────────────────────────

const escposLineWidth = 48 // 80mm paper, Font A

func renderTicketESCPOS(nombreComercial, direccion, telefono, pedidoID, mesa, solicitadoPor string, total float64, items []TicketItem, mensaje, leyenda, qrData string) []byte {
	var b bytes.Buffer

	// ESC/POS command helpers
	init := []byte{0x1b, 0x40}                  // ESC @ — initialize
	center := []byte{0x1b, 0x61, 0x01}           // ESC a 1
	left := []byte{0x1b, 0x61, 0x00}             // ESC a 0
	boldOn := []byte{0x1b, 0x45, 0x01}           // ESC E 1
	boldOff := []byte{0x1b, 0x45, 0x00}          // ESC E 0
	dblHeight := []byte{0x1b, 0x21, 0x10}        // double height
	dblSize := []byte{0x1b, 0x21, 0x30}          // double width + height
	normal := []byte{0x1b, 0x21, 0x00}           // normal
	cut := []byte{0x1d, 0x56, 0x01}              // GS V 1 — partial cut
	lf := []byte{0x0a}

	separator := bytes.Repeat([]byte{'-'}, escposLineWidth)

	// ── Initialize ──
	b.Write(init)

	// ── Header: business name (centered, bold, double height) ──
	b.Write(center)
	b.Write(dblHeight)
	b.Write(boldOn)
	b.Write([]byte(nombreComercial))
	b.Write(lf)
	b.Write(normal)
	b.Write(boldOff)

	if strings.TrimSpace(direccion) != "" {
		b.Write([]byte(direccion))
		b.Write(lf)
	}
	if strings.TrimSpace(telefono) != "" {
		b.Write([]byte("Tel: " + telefono))
		b.Write(lf)
	}

	// ── Separator ──
	b.Write(separator)
	b.Write(lf)

	// ── Ticket title ──
	b.Write(boldOn)
	b.Write([]byte("TICKET DE VENTA"))
	b.Write(lf)
	b.Write(boldOff)

	b.Write([]byte("Pedido: #" + pedidoID))
	b.Write(lf)
	b.Write([]byte("Mesa: " + mesa))
	b.Write(lf)
	if strings.TrimSpace(solicitadoPor) != "" {
		b.Write([]byte("Solicito: " + solicitadoPor))
		b.Write(lf)
	}
	b.Write([]byte("Fecha: " + time.Now().Format("2006-01-02 15:04:05")))
	b.Write(lf)

	// ── Separator ──
	b.Write(separator)
	b.Write(lf)

	// ── Column header ──
	b.Write(left)
	b.Write(boldOn)
	b.Write([]byte(escposColumns("DESCRIPCION", "CANT", "PRECIO")))
	b.Write(lf)
	b.Write(boldOff)

	// ── Items ──
	for _, item := range items {
		qty := strconv.Itoa(item.Cantidad)
		price := fmt.Sprintf("$%.2f", item.Subtotal)
		b.Write([]byte(escposColumns(item.Nombre, qty, price)))
		b.Write(lf)

		for _, ex := range item.Extras {
			exQty := strconv.Itoa(ex.Cantidad)
			exPrice := fmt.Sprintf("$%.2f", ex.Precio)
			exName := "  + " + exQty + "x " + ex.Nombre
			b.Write([]byte(escposColumnsExtra(exName, exPrice)))
			b.Write(lf)
		}

		if strings.TrimSpace(item.Observacion) != "" {
			b.Write([]byte("  Obs: " + item.Observacion))
			b.Write(lf)
		}
	}

	if len(items) == 0 {
		b.Write([]byte("(sin detalles)"))
		b.Write(lf)
	}

	// ── Separator ──
	b.Write(separator)
	b.Write(lf)

	// ── Total (bold, double size) ──
	b.Write(center)
	b.Write(dblSize)
	b.Write(boldOn)
	b.Write([]byte(fmt.Sprintf("TOTAL: $%.2f", total)))
	b.Write(lf)
	b.Write(normal)
	b.Write(boldOff)

	// ── Separator ──
	b.Write(separator)
	b.Write(lf)

	// ── Footer messages ──
	if strings.TrimSpace(mensaje) != "" {
		b.Write([]byte(mensaje))
		b.Write(lf)
	}
	if strings.TrimSpace(leyenda) != "" {
		b.Write([]byte(leyenda))
		b.Write(lf)
	}

	b.Write(lf)
	b.Write([]byte("GRACIAS POR SU PREFERENCIA!"))
	b.Write(lf)
	b.Write(lf)

	// ── QR Code (ESC/POS GS ( k) ──
	if strings.TrimSpace(qrData) != "" {
		b.Write(center)
		// Select model 2
		b.Write([]byte{0x1d, 0x28, 0x6b, 0x04, 0x00, 0x31, 0x41, 0x32, 0x00})
		// Set module size (8 = ~large)
		b.Write([]byte{0x1d, 0x28, 0x6b, 0x03, 0x00, 0x31, 0x43, 0x08})
		// Error correction level M (49)
		b.Write([]byte{0x1d, 0x28, 0x6b, 0x03, 0x00, 0x31, 0x45, 0x31})
		// Store data: pL+pH*256 = len(qrData)+3
		dataLen := len(qrData) + 3
		pL := byte(dataLen & 0xFF)
		pH := byte((dataLen >> 8) & 0xFF)
		b.Write([]byte{0x1d, 0x28, 0x6b, pL, pH, 0x31, 0x50, 0x30})
		b.Write([]byte(qrData))
		// Print stored QR
		b.Write([]byte{0x1d, 0x28, 0x6b, 0x03, 0x00, 0x31, 0x51, 0x30})
		b.Write(lf)
		b.Write(lf)
		b.Write(left)
	}

	b.Write(lf)
	b.Write(lf)

	// ── Cut ──
	b.Write(cut)

	return b.Bytes()
}

// ── ZPL renderer ───────────────────────────────────────────────────────

func renderTicketZPL(nombreComercial, direccion, telefono, pedidoID, mesa, solicitadoPor string, total float64, items []TicketItem, mensaje, leyenda, qrData string) []byte {
	var b strings.Builder
	y := 20

	w := func(s string) { b.WriteString(s) }
	line := func(label string, fontSize, fontW int) {
		w(fmt.Sprintf("^FO10,%d^A0N,%d,%d^FD%s^FS", y, fontSize, fontW, zplSafe(label)))
		y += fontSize + 6
	}
	sep := func() {
		w(fmt.Sprintf("^FO10,%d^GB556,1,2^FS", y))
		y += 8
	}

	// Header placeholder — label length set at the end
	// ^PW640 = 80mm at 203 DPI (RIBETEC standard)
	w("^XA^PW640")

	// ── Header ──
	line(nombreComercial, 28, 16)
	if strings.TrimSpace(direccion) != "" {
		line(direccion, 18, 10)
	}
	if strings.TrimSpace(telefono) != "" {
		line("Tel: "+telefono, 18, 10)
	}
	sep()

	// ── Order info ──
	line("TICKET DE VENTA", 22, 12)
	line("Pedido: #"+pedidoID, 18, 10)
	line("Mesa: "+mesa, 18, 10)
	if strings.TrimSpace(solicitadoPor) != "" {
		line("Solicito: "+solicitadoPor, 18, 10)
	}
	line("Fecha: "+time.Now().Format("2006-01-02 15:04:05"), 18, 10)
	sep()

	// ── Column headers ──
	w(fmt.Sprintf("^FO10,%d^A0N,18,10^FDDESCRIPCION^FS", y))
	w(fmt.Sprintf("^FO400,%d^A0N,18,10^FDCANT^FS", y))
	w(fmt.Sprintf("^FO500,%d^A0N,18,10^FDPRECIO^FS", y))
	y += 24

	// ── Items ──
	for _, item := range items {
		name := item.Nombre
		if len(name) > 24 {
			name = name[:24]
		}
		w(fmt.Sprintf("^FO10,%d^A0N,18,10^FD%s^FS", y, zplSafe(name)))
		w(fmt.Sprintf("^FO400,%d^A0N,18,10^FD%d^FS", y, item.Cantidad))
		w(fmt.Sprintf("^FO500,%d^A0N,18,10^FD$%.2f^FS", y, item.Subtotal))
		y += 22
		for _, ex := range item.Extras {
			exName := "+ " + ex.Nombre
			if len(exName) > 26 {
				exName = exName[:26]
			}
			w(fmt.Sprintf("^FO20,%d^A0N,16,9^FD%s^FS", y, zplSafe(exName)))
			w(fmt.Sprintf("^FO500,%d^A0N,16,9^FD$%.2f^FS", y, ex.Precio))
			y += 20
		}
		if strings.TrimSpace(item.Observacion) != "" {
			w(fmt.Sprintf("^FO20,%d^A0N,16,9^FDObs: %s^FS", y, zplSafe(item.Observacion)))
			y += 20
		}
	}
	if len(items) == 0 {
		line("(sin detalles)", 18, 10)
	}
	sep()

	// ── Total ──
	w(fmt.Sprintf("^FO10,%d^A0N,28,16^FDTOTAL: $%.2f^FS", y, total))
	y += 36

	// ── Footer ──
	if strings.TrimSpace(mensaje) != "" {
		line(mensaje, 18, 10)
	}
	if strings.TrimSpace(leyenda) != "" {
		line(leyenda, 16, 9)
	}
	line("GRACIAS POR SU PREFERENCIA!", 18, 10)
	y += 6

	// ── QR Code ──
	if strings.TrimSpace(qrData) != "" {
		w(fmt.Sprintf("^FO180,%d^BQN,2,5^FDLA,%s^FS", y, zplSafe(qrData)))
		y += 130
	}

	w("^XZ")

	// Inject ^LL (label length) after ^PW576
	labelLen := y + 20
	full := strings.Replace(b.String(), "^XA^PW640", fmt.Sprintf("^XA^PW640^LL%d^MNY", labelLen), 1)
	return []byte(full)
}

func zplSafe(s string) string {
	s = strings.ReplaceAll(s, "^", "")
	s = strings.ReplaceAll(s, "~", "")
	return s
}

// escposColumns formats a 3-column row: name (flexible) | qty (6 chars) | price (10 chars)
func escposColumns(name, qty, price string) string {
	const qtyW = 6
	const priceW = 10
	nameW := escposLineWidth - qtyW - priceW
	if len(name) > nameW {
		name = name[:nameW]
	}
	return fmt.Sprintf("%-*s%*s%*s", nameW, name, qtyW, qty, priceW, price)
}

// escposColumnsExtra formats a 2-column row for extras: name (left) | price (right-aligned)
func escposColumnsExtra(name, price string) string {
	const priceW = 10
	nameW := escposLineWidth - priceW
	if len(name) > nameW {
		name = name[:nameW]
	}
	return fmt.Sprintf("%-*s%*s", nameW, name, priceW, price)
}

func escapeHTML(value string) string {
	replacer := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&#39;")
	return replacer.Replace(value)
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

func numberFromAny(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case float32:
		return float64(x)
	case int:
		return float64(x)
	case int64:
		return float64(x)
	case json.Number:
		f, _ := x.Float64()
		return f
	case string:
		f, _ := strconv.ParseFloat(strings.TrimSpace(x), 64)
		return f
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
