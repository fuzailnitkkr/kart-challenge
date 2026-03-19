import { useEffect, useMemo, useState } from 'react'

const API_BASE_URL = (import.meta.env.VITE_API_BASE_URL || 'http://localhost:8080').trim()
const API_KEY = (import.meta.env.VITE_API_KEY || 'apitest').trim()
const DEVICE_ID_HEADER = (import.meta.env.VITE_DEVICE_ID_HEADER || 'X-Device-ID').trim()
const USER_ID_HEADER = (import.meta.env.VITE_USER_ID_HEADER || 'X-User-ID').trim()

const DEVICE_STORAGE_KEY = 'kart_challenge_device_id_v1'
const USER_STORAGE_KEY = 'kart_challenge_user_id_v1'

function buildApiPath(path) {
  const base = API_BASE_URL.replace(/\/$/, '')
  return `${base}${path}`
}

function createStableId(prefix) {
  if (window.crypto?.randomUUID) {
    return `${prefix}-${window.crypto.randomUUID()}`
  }
  return `${prefix}-${Date.now()}-${Math.random().toString(16).slice(2, 10)}`
}

function ensureClientIdentity() {
  let deviceId = localStorage.getItem(DEVICE_STORAGE_KEY)
  if (!deviceId) {
    deviceId = createStableId('device')
    localStorage.setItem(DEVICE_STORAGE_KEY, deviceId)
  }

  let userId = localStorage.getItem(USER_STORAGE_KEY)
  if (!userId) {
    const suffix = deviceId.slice(-8)
    userId = `user-${suffix}`
    localStorage.setItem(USER_STORAGE_KEY, userId)
  }

  return { deviceId, userId }
}

async function requestJson(path, { method = 'GET', body, deviceId, userId } = {}) {
  const headers = new Headers()
  headers.set('Accept', 'application/json')
  headers.set('api_key', API_KEY)
  headers.set(DEVICE_ID_HEADER, deviceId)
  headers.set(USER_ID_HEADER, userId)

  const options = { method, headers }
  if (body !== undefined) {
    headers.set('Content-Type', 'application/json')
    options.body = JSON.stringify(body)
  }

  const response = await fetch(buildApiPath(path), options)
  const text = await response.text()

  let payload = {}
  if (text) {
    try {
      payload = JSON.parse(text)
    } catch {
      payload = {}
    }
  }

  if (!response.ok) {
    const message = payload.message || payload.error || `Request failed (${response.status})`
    const error = new Error(message)
    error.status = response.status
    error.payload = payload
    throw error
  }

  return payload
}

function formatPrice(value) {
  return `$${Number(value || 0).toFixed(2)}`
}

function normalizeCoupon(raw) {
  return (raw || '').trim().toUpperCase()
}

function getDiscountPreview(code, cartItems, subtotal) {
  if (!code || !cartItems.length) {
    return { amount: 0, label: '' }
  }

  if (code === 'HAPPYHOURS') {
    const amount = subtotal * 0.18
    return {
      amount,
      label: '18% off (HAPPYHOURS)',
    }
  }

  if (code === 'BUYGETONE') {
    let cheapestUnitPrice = Number.POSITIVE_INFINITY
    cartItems.forEach((item) => {
      if (item.quantity > 0 && item.price < cheapestUnitPrice) {
        cheapestUnitPrice = item.price
      }
    })
    if (Number.isFinite(cheapestUnitPrice)) {
      return {
        amount: cheapestUnitPrice,
        label: 'Lowest priced item free (BUYGETONE)',
      }
    }
  }

  return { amount: 0, label: '' }
}

function ProductCard({ product, quantity, onAdd, onIncrement, onDecrement }) {
  const isSelected = quantity > 0

  return (
    <article className="product-card">
      <div className={`product-image-wrap ${isSelected ? 'selected' : ''}`}>
        <picture>
          <source media="(max-width: 720px)" srcSet={product.image?.mobile || product.image?.thumbnail} />
          <source media="(max-width: 1100px)" srcSet={product.image?.tablet || product.image?.desktop} />
          <img src={product.image?.desktop || product.image?.thumbnail} alt={product.name} loading="lazy" />
        </picture>
      </div>

      <div className="product-action-row">
        {!isSelected ? (
          <button className="add-to-cart-btn" type="button" onClick={onAdd}>
            <img src="/assets/icon-add-to-cart.png" alt="" aria-hidden="true" />
            Add to Cart
          </button>
        ) : (
          <div className="quantity-control" role="group" aria-label={`Quantity for ${product.name}`}>
            <button type="button" onClick={onDecrement} aria-label={`Decrease ${product.name}`}>
              <img src="/assets/icon-decrement.png" alt="" aria-hidden="true" />
            </button>
            <span>{quantity}</span>
            <button type="button" onClick={onIncrement} aria-label={`Increase ${product.name}`}>
              <img src="/assets/icon-increment.png" alt="" aria-hidden="true" />
            </button>
          </div>
        )}
      </div>

      <div className="product-meta">
        <p className="product-category">{product.category}</p>
        <h2>{product.name}</h2>
        <p className="product-price">{formatPrice(product.price)}</p>
      </div>
    </article>
  )
}

function App() {
  const [products, setProducts] = useState([])
  const [cart, setCart] = useState({})
  const [loadingProducts, setLoadingProducts] = useState(true)
  const [loadingError, setLoadingError] = useState('')
  const [couponCode, setCouponCode] = useState('')
  const [validatedCouponCode, setValidatedCouponCode] = useState('')
  const [couponMessage, setCouponMessage] = useState('')
  const [couponMessageType, setCouponMessageType] = useState('info')
  const [applyingCoupon, setApplyingCoupon] = useState(false)
  const [submitError, setSubmitError] = useState('')
  const [placingOrder, setPlacingOrder] = useState(false)
  const [confirmation, setConfirmation] = useState(null)

  const [{ deviceId, userId }] = useState(() => ensureClientIdentity())

  useEffect(() => {
    let cancelled = false

    async function loadProducts() {
      try {
        setLoadingProducts(true)
        setLoadingError('')
        const data = await requestJson('/product', { deviceId, userId })
        if (!cancelled) {
          setProducts(Array.isArray(data) ? data : [])
        }
      } catch (error) {
        if (!cancelled) {
          setLoadingError(error.message || 'Failed to load products')
        }
      } finally {
        if (!cancelled) {
          setLoadingProducts(false)
        }
      }
    }

    loadProducts()

    return () => {
      cancelled = true
    }
  }, [deviceId, userId])

  const cartItems = useMemo(() => {
    return Object.entries(cart)
      .map(([productId, quantity]) => {
        const product = products.find((item) => String(item.id) === productId)
        if (!product || quantity <= 0) return null
        return {
          ...product,
          quantity,
          lineTotal: quantity * Number(product.price || 0),
        }
      })
      .filter(Boolean)
  }, [cart, products])

  const cartCount = useMemo(() => cartItems.reduce((sum, item) => sum + item.quantity, 0), [cartItems])
  const subtotal = useMemo(() => cartItems.reduce((sum, item) => sum + item.lineTotal, 0), [cartItems])

  const normalizedCoupon = normalizeCoupon(couponCode)
  const activeCouponCode = normalizedCoupon === validatedCouponCode ? normalizedCoupon : ''
  const discount = useMemo(
    () => getDiscountPreview(activeCouponCode, cartItems, subtotal),
    [activeCouponCode, cartItems, subtotal],
  )
  const total = Math.max(0, subtotal - discount.amount)

  function setProductQuantity(productId, nextQuantity) {
    setSubmitError('')
    setCart((prev) => {
      const updated = { ...prev }
      if (nextQuantity <= 0) {
        delete updated[productId]
      } else {
        updated[productId] = nextQuantity
      }
      return updated
    })
  }

  function increment(productId) {
    const current = cart[productId] || 0
    setProductQuantity(productId, current + 1)
  }

  function decrement(productId) {
    const current = cart[productId] || 0
    setProductQuantity(productId, current - 1)
  }

  function handleCouponChange(value) {
    setSubmitError('')
    setCouponMessage('')
    setCouponMessageType('info')
    const nextNormalized = normalizeCoupon(value)
    if (nextNormalized !== validatedCouponCode) {
      setValidatedCouponCode('')
    }
    setCouponCode(value)
  }

  async function applyCoupon() {
    if (applyingCoupon) return

    const code = normalizedCoupon
    if (!code) {
      setValidatedCouponCode('')
      setCouponMessageType('error')
      setCouponMessage('Enter a coupon code to apply.')
      return
    }

    setSubmitError('')
    setCouponMessage('')
    setCouponMessageType('info')
    setApplyingCoupon(true)

    try {
      const response = await requestJson('/coupon/validate', {
        method: 'POST',
        body: { couponCode: code },
        deviceId,
        userId,
      })

      if (!response.valid) {
        setValidatedCouponCode('')
        setCouponMessageType('error')
        setCouponMessage(response.message || 'Coupon is invalid.')
        return
      }

      const normalized = normalizeCoupon(response.couponCode || code)
      setValidatedCouponCode(normalized)
      setCouponCode(normalized)
      setCouponMessageType('success')
      setCouponMessage(response.message || 'Coupon applied successfully.')
    } catch (error) {
      setValidatedCouponCode('')
      setCouponMessageType('error')
      setCouponMessage(error.message || 'Unable to validate coupon right now')
    } finally {
      setApplyingCoupon(false)
    }
  }

  async function confirmOrder() {
    if (!cartItems.length || placingOrder) return

    setSubmitError('')
    setPlacingOrder(true)

    const items = cartItems.map((item) => ({
      productId: String(item.id),
      quantity: item.quantity,
    }))

    const payload = { items }
    if (activeCouponCode) {
      payload.couponCode = activeCouponCode
    }

    try {
      const response = await requestJson('/order', {
        method: 'POST',
        body: payload,
        deviceId,
        userId,
      })

      setConfirmation({
        orderId: response.id,
        items: cartItems,
        subtotal,
        discountAmount: discount.amount,
        discountLabel: discount.label,
        total,
      })
    } catch (error) {
      setSubmitError(error.message || 'Unable to place order')
    } finally {
      setPlacingOrder(false)
    }
  }

  function startNewOrder() {
    setCart({})
    setCouponCode('')
    setValidatedCouponCode('')
    setCouponMessage('')
    setCouponMessageType('info')
    setSubmitError('')
    setConfirmation(null)
  }

  return (
    <div className="app-shell">
      <header className="page-header">
        <h1>Desserts</h1>
      </header>

      <main className="page-layout">
        <section className="products-grid" aria-label="Products">
          {loadingProducts ? (
            Array.from({ length: 9 }).map((_, index) => (
              <article className="product-card skeleton" key={`skeleton-${index}`}>
                <div className="skeleton-image" />
                <div className="skeleton-line short" />
                <div className="skeleton-line" />
                <div className="skeleton-line tiny" />
              </article>
            ))
          ) : loadingError ? (
            <div className="load-error" role="alert">
              <p>{loadingError}</p>
              <button type="button" onClick={() => window.location.reload()}>
                Retry
              </button>
            </div>
          ) : (
            products.map((product) => (
              <ProductCard
                key={product.id}
                product={product}
                quantity={cart[String(product.id)] || 0}
                onAdd={() => increment(String(product.id))}
                onIncrement={() => increment(String(product.id))}
                onDecrement={() => decrement(String(product.id))}
              />
            ))
          )}
        </section>

        <aside className="cart-panel" aria-label="Cart summary">
          <h2>Your Cart ({cartCount})</h2>

          {!cartItems.length ? (
            <div className="empty-cart-state">
              <img src="/assets/empty-cart.png" alt="" aria-hidden="true" />
              <p>Your added items will appear here</p>
            </div>
          ) : (
            <>
              <ul className="cart-list">
                {cartItems.map((item) => (
                  <li key={item.id} className="cart-item-row">
                    <div>
                      <p className="cart-item-name">{item.name}</p>
                      <p className="cart-item-meta">
                        <span>{item.quantity}x</span>
                        <small>@ {formatPrice(item.price)}</small>
                        <strong>{formatPrice(item.lineTotal)}</strong>
                      </p>
                    </div>
                    <button
                      type="button"
                      className="remove-item-btn"
                      onClick={() => setProductQuantity(String(item.id), 0)}
                      aria-label={`Remove ${item.name}`}
                    >
                      <img src="/assets/icon-remove-item.png" alt="" aria-hidden="true" />
                    </button>
                  </li>
                ))}
              </ul>

              <div className="cart-total-row">
                <span>Order Total</span>
                <strong>{formatPrice(total)}</strong>
              </div>

              <div className="carbon-note">
                <img src="/assets/carbon-neutral.png" alt="" aria-hidden="true" />
                <p>
                  This is a <strong>carbon-neutral</strong> delivery
                </p>
              </div>

              <div className="coupon-row">
                <input
                  type="text"
                  value={couponCode}
                  onChange={(event) => handleCouponChange(event.target.value)}
                  placeholder="Coupon code (optional)"
                  aria-label="Coupon code"
                />
                <button
                  type="button"
                  className="apply-coupon-btn"
                  onClick={applyCoupon}
                  disabled={applyingCoupon || !normalizedCoupon}
                >
                  {applyingCoupon ? 'Applying...' : 'Apply Coupon'}
                </button>
              </div>

              {couponMessage && (
                <p
                  className={`coupon-feedback ${couponMessageType === 'success' ? 'success' : 'error'}`}
                  role={couponMessageType === 'error' ? 'alert' : undefined}
                >
                  {couponMessage}
                </p>
              )}

              {discount.amount > 0 && (
                <p className="discount-note">
                  {discount.label}: -{formatPrice(discount.amount)}
                </p>
              )}

              {submitError && (
                <p className="submit-error" role="alert">
                  {submitError}
                </p>
              )}

              <button className="confirm-order-btn" type="button" onClick={confirmOrder} disabled={placingOrder}>
                {placingOrder ? 'Placing Order...' : 'Confirm Order'}
              </button>
            </>
          )}
        </aside>
      </main>

      {confirmation && (
        <div className="modal-backdrop" role="dialog" aria-modal="true" aria-label="Order confirmation">
          <div className="confirmation-modal">
            <img src="/assets/order-confirmed.png" alt="" aria-hidden="true" className="confirmed-icon" />
            <h3>Order Confirmed</h3>
            <p>We hope you enjoy your food!</p>

            <div className="confirmation-items-wrap">
              <ul className="confirmation-items-list">
                {confirmation.items.map((item) => (
                  <li key={`confirmation-${item.id}`} className="confirmation-item-row">
                    <img src={item.image?.thumbnail || item.image?.desktop} alt="" aria-hidden="true" />
                    <div>
                      <p>{item.name}</p>
                      <span>
                        {item.quantity}x <small>@ {formatPrice(item.price)}</small>
                      </span>
                    </div>
                    <strong>{formatPrice(item.lineTotal)}</strong>
                  </li>
                ))}
              </ul>

              {confirmation.discountAmount > 0 && (
                <div className="confirmation-summary-row minor">
                  <span>Discount</span>
                  <strong>-{formatPrice(confirmation.discountAmount)}</strong>
                </div>
              )}

              <div className="confirmation-summary-row">
                <span>Order Total</span>
                <strong>{formatPrice(confirmation.total)}</strong>
              </div>
            </div>

            <button className="start-new-order-btn" type="button" onClick={startNewOrder}>
              Start New Order
            </button>
          </div>
        </div>
      )}
    </div>
  )
}

export default App
