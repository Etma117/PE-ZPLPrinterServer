# POS Print Microservice (Go)

Microservicio en Go para ejecutar en segundo plano.

Funciones:
- Inicia sesion en el backend con un usuario real de `Usuarios`.
- Consulta la cola de impresion remota (`pending`).
- Imprime cada trabajo (modo `file`, `lp` o `command`).
- Marca cada trabajo como `IMPRESO` o `ERROR`.

## Seguridad

Este servicio usa `Bearer token` de un usuario del sistema.

El usuario **debe** tener permisos de impresion:
- Puesto con ruta `/sistema/impresoras-termicas`, o
- `__ALL__`, o
- Rol admin/superusuario.

No usa API key tecnica para la cola micro.

## Configuracion

1. Copia `.env.example` a `.env`.
2. Ajusta valores:
- `BACKEND_URL`
- `PRINT_USER`
- `PRINT_PASSWORD`
- `PRINT_BACKEND` (`file`, `lp`, `command`)

## Ejecucion local

```bash
go run ./cmd/printspooler
```

## Compilar binario

```bash
go build -o printspooler ./cmd/printspooler
./printspooler
```

## Ejecutar en segundo plano (Linux/macOS)

```bash
nohup ./printspooler > printspooler.log 2>&1 &
```

## Docker

```bash
docker build -t pos-print-spooler .
docker run --rm --env-file .env pos-print-spooler
```

## Modo de impresion

- `PRINT_BACKEND=file`
  - Guarda tickets de texto en `OUTPUT_DIR`.
  - Ideal para pruebas.

- `PRINT_BACKEND=lp`
  - Envia al sistema CUPS (`lp`).
  - Usa `PRINTER_NAME` opcional.

- `PRINT_BACKEND=command`
  - Ejecuta `PRINT_COMMAND` y pasa el ticket por `stdin`.
  - Util para scripts personalizados.

## Endpoints usados

- `POST /api/login`
- `GET /api/impresoras-termicas/micro/pool/pending`
- `POST /api/impresoras-termicas/micro/pool/:id/printed`
- `POST /api/impresoras-termicas/micro/pool/:id/error`

## Notas

- Si el token expira, el microservicio reloguea automaticamente.
- Procesa trabajos de forma secuencial por seguridad.
- Para alta disponibilidad con multiples instancias, se recomienda agregar endpoint de `claim` atomico en backend.
