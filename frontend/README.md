# Kart Challenge Frontend (React + Vite)

Figma-driven React implementation for the shopping cart challenge.

## Features

- Product listing with responsive images
- Add/remove/increment/decrement cart actions
- Cart summary with totals and removable line items
- Optional coupon input with preview logic for:
  - `HAPPYHOURS` (18% off)
  - `BUYGETONE` (lowest-priced item free)
- Order placement via API (`POST /order`)
- Order confirmation modal with purchased items
- Responsive behavior for desktop and mobile layouts

## API Headers Used

All API requests send:

- `api_key`
- `X-Device-ID` (configurable)
- `X-User-ID` (configurable)

## Environment Variables

Copy `.env.example` to `.env` and adjust if needed:

- `VITE_API_BASE_URL` (default in app: `http://localhost:8080`)
- `VITE_API_KEY` (default in app: `apitest`)
- `VITE_DEVICE_ID_HEADER` (default in app: `X-Device-ID`)
- `VITE_USER_ID_HEADER` (default in app: `X-User-ID`)
- `VITE_SEND_COUPON_TO_API` (default in app: `false`; keeps coupon logic frontend-only)

## Run

```bash
cd /Users/fuzail/Documents/workspace/kart-challenge/frontend
npm install
npm run dev
```

Open `http://localhost:5173`.

## Production Build

```bash
npm run build
npm run preview
```
