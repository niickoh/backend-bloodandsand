# Blood & Sand · API (Go)

Monolito modular en Go: reservas de clases, planes, pagos (Webpay Plus, Mercado Pago y un proveedor de prueba),
cuentas con perfiles **admin / coach / alumno** y notificaciones Web Push.

- Go 1.25+, solo `net/http` de la librería estándar (ruteo con patrones `GET /api/x/{id}`), `log/slog`.
- PostgreSQL **10 o superior** (`pgx/v5`). El SQL evita lo que PG 10 no soporta.
- Binario único y estático: migraciones, zona horaria y todo lo demás van incluidos.

## Correr en local

```bash
cp .env.example .env              # completa JWT_SECRET, ADMIN_PASSWORD y DATABASE_URL
go run ./cmd/vapidgen >> .env     # claves para Web Push (una sola vez)

docker compose up -d db           # PostgreSQL 10 en :55432, o usa tu propia base

go run ./cmd/api -seed-demo       # migra, crea coaches/alumno/horario de ejemplo y arranca en :8088
go run ./cmd/api                  # arranques siguientes
```

En el front, `npm start` ya trae un proxy de `/api` a `localhost:8088`.

Usuarios de `-seed-demo` (contraseña `demo1234`): coaches `camila@`, `diego@`, `tomas@`, `javiera@` y el alumno `alumno@bloodandsand.cl`.
El admin se crea con `ADMIN_EMAIL` y `ADMIN_PASSWORD` del `.env`.

> En Windows, si `go build` reclama por el estado VCS (un repo git en `C:\`), usa `GOFLAGS=-buildvcs=false`.

## Tests

```bash
go test ./...                                                     # unitarios
TEST_DATABASE_URL=postgres://.../bloodandsand_test go test -race ./...   # + integración (TRUNCA esa base)
```

Los de integración cubren: 12 reservas simultáneas por 3 cupos (entran exactamente 3), primera clase gratis y
después pagada, y que un pago fallido libera el cupo retenido.

## Estructura

```
cmd/api            main: config, señales, apagado ordenado, flags -migrate y -seed-demo
cmd/vapidgen       genera claves VAPID
migrations/        SQL embebido en el binario; se aplica solo al arrancar (con advisory lock)
internal/
  app/             wiring: crea los servicios, arma las rutas y los workers
  config/          variables de entorno validadas
  platform/        piezas sin lógica de negocio
    actor/         quién llama (id + rol) en el context
    apperr/        errores de dominio → HTTP con mensaje para el usuario y errores por campo
    db/            pool, transacciones, migraciones
    httpx/         JSON, middlewares (logs, recover, CORS, CSRF, rate limit), router con guards por rol
    security/      bcrypt, tokens aleatorios
  auth/            login, registro, refresh rotativo con detección de reuso, cookies httpOnly
  user/            cuentas y administración de usuarios
  catalog/         disciplinas y planes
  schedule/        horario semanal (plantillas) y clases concretas; generador idempotente
  membership/      planes comprados y clases disponibles
  booking/         precios, cupos con bloqueo de fila, cancelaciones, CRUD de staff, recordatorios
  payment/         órdenes y proveedores (fake, webpay, mercadopago) detrás de una interfaz
  notify/          suscripciones push y envío (formato del service worker de Angular)
  dashboard/       números para el panel de admin
```

Cada módulo tiene su `Service` (reglas + SQL) y su `http.go` (rutas). Las dependencias van en una sola dirección:
`payment → booking → schedule/membership → catalog`. Las operaciones que tocan varios módulos (reservar con plan,
confirmar un pago) corren en una sola transacción: los repositorios reciben un `db.Querier` que puede ser el pool o un `pgx.Tx`.

## Reglas de negocio

| Regla | Dónde |
|---|---|
| Primera clase gratis por persona (cuenta o correo); la evaluación siempre es gratis | `booking.Price` |
| Con plan activo y clases disponibles, la reserva usa una clase | `booking.Price`, `membership.UseCredit` |
| Ventana de reserva: 14 días (o la del plan, p. ej. 7 en Plan 8) | `booking.checkBookable`, `checkPlanWindow` |
| Cupos: `SELECT … FOR UPDATE` sobre la clase antes de contar e insertar | `booking.insert` |
| Clase pagada: el cupo queda retenido 15 min mientras se paga; si falla o expira, se libera | `booking.Hold`, `payment.Fail`, worker `expirar-pagos` |
| Cancelar con más de 3 h devuelve la clase del plan; staff siempre la devuelve | `booking.CancelOwn`, `changeStatus` |
| Coach: solo ve y edita reservas de **sus** clases; admin, todas | `booking.authorizeSession` |
| Cambiar la hora de una clase o cancelarla avisa por push a los inscritos | `booking.UpdateSession`, `CancelSession` |
| Recordatorio push 2 h antes, una sola vez (claim atómico, seguro con varias instancias) | `booking.SendReminders` |
| Registrarse vincula las reservas y pagos hechos antes como invitado con ese correo | `auth.Register` |

## Sesión y seguridad

- `bs_access`: JWT HS256 de 15 min (cookie httpOnly, SameSite=Lax). `bs_refresh`: token opaco de 30 días, guardado
  como SHA-256, **rotado en cada uso**; reutilizar uno viejo revoca toda la familia de sesiones.
- CSRF: las escrituras exigen `Content-Type: application/json` (o `X-Requested-With`), que un sitio ajeno no puede
  enviar sin un preflight CORS, que solo se concede a `CORS_ORIGINS`. Los callbacks de pago quedan exentos.
- Rate limit en login, registro y cambio de contraseña. bcrypt costo 12. Mismo tiempo de respuesta exista o no el correo.
- Cambiar el rol, desactivar una cuenta o cambiar su contraseña cierra las sesiones de esa persona.

## Pagos

`PAYMENT_PROVIDERS=fake,webpay,mercadopago`: el front muestra los que estén activos.

- **fake**: tarjeta de prueba dentro de la app (`4242…` aprueba, `…0002` rechaza). Bloqueado en producción.
- **Webpay Plus**: viene con las credenciales públicas de integración de Transbank. Flujo: crear transacción →
  el navegador hace POST a Webpay → vuelve a `/api/payments/webpay/return` → *commit* → redirige a `/pago/resultado`.
  Para producción: `WEBPAY_ENV=production` y tu código de comercio y API key.
- **Mercado Pago Checkout Pro**: `MP_ACCESS_TOKEN` (TEST-… en sandbox). Webhook en
  `/api/payments/mercadopago/webhook` con verificación de `x-signature` si defines `MP_WEBHOOK_SECRET`.
  El pago siempre se vuelve a consultar en la API de MP; nunca se confía en los parámetros de la URL.
  MP exige URLs HTTPS públicas para webhooks y `auto_return` (en local usa un túnel, p. ej. `cloudflared`).
- Reembolsos desde el panel: Webpay y MP se reembolsan con su API; el resto se marca como manual.

## Endpoints

Público: `GET /api/catalog` · `GET /api/timetable` · `GET /api/sessions?from&to&discipline` ·
`POST /api/bookings/quote` · `POST /api/bookings` · `POST /api/checkout/class` · `GET /api/orders/{id}?token=` ·
`GET /api/payments/providers` · `GET /api/push/public-key` · `POST|DELETE /api/push/subscriptions`

Auth: `POST /api/auth/{login,register,refresh,logout,password}` · `GET|PATCH /api/auth/me`

Alumno: `GET /api/me/bookings` · `POST /api/me/bookings/{id}/cancel` · `GET /api/me/membership` · `GET /api/me/orders` ·
`POST /api/checkout/plan`

Coach y admin: `GET /api/coach/sessions` · `GET /api/staff/sessions/{id}/roster` · `POST /api/staff/sessions/{id}/bookings` ·
`PATCH|DELETE /api/staff/bookings/{id}` · `GET /api/staff/students?q=`

Admin: `/api/admin/` + `dashboard`, `users[/{id}[/password|/memberships]]`, `memberships/{id}`, `coaches`,
`disciplines[/{id}]`, `plans[/{id}]`, `templates[/{id}]`, `sessions[/generate|/{id}|/{id}/cancel]`, `bookings`,
`orders[/{id}|/{id}/refund]`, `announcements`

Errores: `{"error": "session_full", "message": "La clase se llenó. Elige otro horario.", "fields": {...}}`.

## Despliegue (VPS con systemd + nginx, igual que sphere-go)

```bash
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -buildvcs=false -ldflags="-s -w" -o api ./cmd/api

sudo useradd --system --no-create-home bloodandsand
sudo mkdir -p /opt/bloodandsand-api && sudo cp api .env /opt/bloodandsand-api/
sudo chown -R bloodandsand: /opt/bloodandsand-api && sudo chmod 600 /opt/bloodandsand-api/.env
sudo cp deploy/bloodandsand-api.service /etc/systemd/system/ && sudo systemctl enable --now bloodandsand-api

# front en /var/www/bloodandsand/browser (npm run build) y deploy/nginx.conf como sitio
sudo certbot --nginx -d bloodandsand.cl
```

Front y API en el mismo dominio (nginx sirve `/` y pasa `/api/` a Go): las cookies son de primera parte y no hace falta CORS.
En producción: `APP_ENV=production`, `PAYMENT_PROVIDERS=webpay` (o `mercadopago`), `FRONTEND_URL` y `PUBLIC_API_URL` = `https://bloodandsand.cl`.
