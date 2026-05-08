import { useEffect, useRef } from 'react'
import { createChart, ColorType, IChartApi, ISeriesApi, CandlestickData, Time } from 'lightweight-charts'

interface Props {
  pair: string
  height?: number
}

function pickPrice(payload: Record<string, number>, pair: string): number | undefined {
  if (typeof payload[pair] === 'number') return payload[pair]
  const fallback: Record<string, string> = {
    'v2:BTCUSDT': 'V2 BTC-Beta/USDT-Beta',
    'v3:BTCUSDT': 'V3 BTC-Beta/USDT-Beta',
    'v4:BTCUSDT': 'V4 BTC-Beta/USDT-Beta',
    'v4:ETHUSDT': 'V4 ETH-Beta/USDT-Beta',
    'v4:ETHBTC': 'V4 ETH-Beta/BTC-Beta',
  }
  const key = fallback[pair]
  return key ? payload[key] : undefined
}

export function KLineChart({ pair, height = 300 }: Props) {
  const containerRef = useRef<HTMLDivElement>(null)
  const chartRef = useRef<IChartApi | null>(null)
  const seriesRef = useRef<ISeriesApi<'Candlestick'> | null>(null)
  const candleDataRef = useRef<CandlestickData[]>([])
  const lastCandleRef = useRef<CandlestickData | null>(null)
  const lastRenderRef = useRef<number>(0)

  const backfillGap = async () => {
    if (!seriesRef.current) return
    const now = Math.floor(Date.now() / 1000)
    const lastTs = (lastCandleRef.current?.time as number) || (now - 3600)
    const from = Math.max(lastTs - 120, now - 24 * 3600)
    try {
      const r = await fetch(`/api/market/klines?pair=${encodeURIComponent(pair)}&interval=1m&from=${from}&to=${now}&_t=${Date.now()}`, { cache: 'no-store' })
      const data = await r.json()
      if (!Array.isArray(data)) return
      const candles: CandlestickData[] = data
        .filter((p: any) => p.open > 0 && p.close > 0)
        .map((p: any) => ({ time: p.time as Time, open: p.open, high: p.high, low: p.low, close: p.close }))
        .sort((a: any, b: any) => (a.time as number) - (b.time as number))
      if (candles.length === 0) return
      const merged = [...candleDataRef.current]
      const lastLocal = merged.length ? (merged[merged.length - 1].time as number) : 0
      for (const c of candles) {
        if ((c.time as number) > lastLocal) merged.push(c)
      }
      while (merged.length > 1000) merged.shift()
      candleDataRef.current = merged
      lastCandleRef.current = merged[merged.length - 1]
      seriesRef.current.setData(merged)
    } catch {
      // ignore intermittent backfill errors
    }
  }

  useEffect(() => {
    if (!containerRef.current) return

    const chart = createChart(containerRef.current!, {
      width: containerRef.current.clientWidth,
      height,
      layout: {
        background: { type: ColorType.Solid, color: '#161b22' },
        textColor: '#8b949e',
      },
      grid: {
        vertLines: { color: '#21262d' },
        horzLines: { color: '#21262d' },
      },
      crosshair: { mode: 0 },
      timeScale: {
        borderColor: '#30363d',
        timeVisible: true,
      },
      rightPriceScale: {
        borderColor: '#30363d',
      },
    })

    const series = chart.addCandlestickSeries({
      upColor: '#3fb950',
      downColor: '#f85149',
      borderUpColor: '#3fb950',
      borderDownColor: '#f85149',
      wickUpColor: '#3fb950',
      wickDownColor: '#f85149',
    })

    chartRef.current = chart
    seriesRef.current = series

    const handleResize = () => {
      if (containerRef.current) {
        chart.applyOptions({ width: containerRef.current.clientWidth })
      }
    }
    window.addEventListener('resize', handleResize)

    return () => {
      window.removeEventListener('resize', handleResize)
      chart.remove()
    }
  }, [height])

  // Fetch historical klines on mount
  useEffect(() => {
    const now = Math.floor(Date.now() / 1000)
    const from = now - 24 * 3600
    fetch(`/api/market/klines?pair=${encodeURIComponent(pair)}&interval=1m&from=${from}&to=${now}&_t=${Date.now()}`, { cache: 'no-store' })
      .then(r => r.json())
      .then((data: any[]) => {
        if (!Array.isArray(data) || !seriesRef.current) return
        const candles: CandlestickData[] = data
          .filter((p: any) => p.open > 0 && p.close > 0)
          .map((p: any) => ({
            time: p.time as Time,
            open: p.open,
            high: p.high,
            low: p.low,
            close: p.close,
          }))
          .sort((a: any, b: any) => (a.time as number) - (b.time as number))
        candleDataRef.current = candles
        if (candles.length > 0) {
          lastCandleRef.current = candles[candles.length - 1]
        }
        seriesRef.current!.setData(candles)
      })
      .catch(() => {})
  }, [pair])

  // HTTP fallback: ensure at least one candle and periodic updates even if WS
  // is unavailable or temporarily reconnecting.
  useEffect(() => {
    let cancelled = false
    const updateFromHttp = async () => {
      try {
        const resp = await fetch(`/api/market/prices?_t=${Date.now()}`, { cache: 'no-store' })
        const prices = await resp.json()
        const price = pickPrice(prices, pair)
        if (!price || !seriesRef.current || cancelled) return

        const ms = Date.now()
        if (ms-lastRenderRef.current < 250) return
        lastRenderRef.current = ms
        const now = Math.floor(ms / 1000) as Time
        const last = lastCandleRef.current
        if (!last || (now as number) - (last.time as number) >= 60) {
          const candle: CandlestickData = {
            time: now,
            open: price,
            high: price,
            low: price,
            close: price,
          }
          candleDataRef.current.push(candle)
          if (candleDataRef.current.length > 1000) candleDataRef.current.shift()
          lastCandleRef.current = candle
          seriesRef.current.setData(candleDataRef.current)
        } else {
          if (price > last.high) last.high = price
          if (price < last.low) last.low = price
          last.close = price
          seriesRef.current.update(last)
        }
      } catch {
        // ignore intermittent network errors
      }
    }

    updateFromHttp()
    const timer = setInterval(updateFromHttp, 1000)
    return () => {
      cancelled = true
      clearInterval(timer)
    }
  }, [pair])

  // WebSocket real-time updates
  useEffect(() => {
    const wsProtocol = location.protocol === 'https:' ? 'wss:' : 'ws:'
    const wsUrl = `${wsProtocol}//${location.host}/ws`
    let ws: WebSocket | null = null
    let reconnectTimer: number | null = null
    let disposed = false

    const connect = () => {
      if (disposed) return
      ws = new WebSocket(wsUrl)
      ws.onopen = () => {
        ws?.send(JSON.stringify({ type: 'subscribe', pairs: [pair] }))
        backfillGap()
      }
      ws.onmessage = (e) => {
        try {
          const prices = JSON.parse(e.data)
          const price = pickPrice(prices, pair)
          if (!price || !seriesRef.current) return

          const ms = Date.now()
          if (ms-lastRenderRef.current < 250) return
          lastRenderRef.current = ms
          const now = Math.floor(ms / 1000) as Time
          const last = lastCandleRef.current

          if (!last || (now as number) - (last.time as number) >= 60) {
            const candle: CandlestickData = {
              time: now,
              open: price,
              high: price,
              low: price,
              close: price,
            }
            candleDataRef.current.push(candle)
            if (candleDataRef.current.length > 1000) candleDataRef.current.shift()
            lastCandleRef.current = candle
            seriesRef.current.setData(candleDataRef.current)
          } else {
            if (price > last.high) last.high = price
            if (price < last.low) last.low = price
            last.close = price
            seriesRef.current.update(last)
          }
        } catch {}
      }
      ws.onerror = () => ws?.close()
      ws.onclose = () => {
        if (!disposed) reconnectTimer = window.setTimeout(connect, 3000)
      }
    }
    connect()
    return () => {
      disposed = true
      if (reconnectTimer) window.clearTimeout(reconnectTimer)
      if (ws?.readyState === WebSocket.OPEN) ws.close()
      else if (ws?.readyState === WebSocket.CONNECTING) ws.onopen = () => ws?.close()
    }
  }, [pair])

  return <div ref={containerRef} style={{ width: '100%', height }} />
}
