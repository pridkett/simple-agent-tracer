export async function request(path, { body, ...options } = {}) {
  const response = await fetch(path, {
    ...options,
    ...(body ? { body: JSON.stringify(body), headers: { 'Content-Type': 'application/json' } } : {}),
  })
  if (response.status === 204) return
  const data = await response.json()
  if (!response.ok) {
    throw Object.assign(new Error(data.error || 'Request failed'), { status: response.status })
  }
  return data
}

export function projectPath(slug) {
  return '/api/v1/projects/' + encodeURIComponent(slug)
}

export function dateLabel(value) {
  return value ? new Intl.DateTimeFormat(undefined, { dateStyle: 'medium', timeStyle: 'medium' }).format(new Date(value)) : '—'
}

export function statusLabel(value) {
  return ({ STATUS_CODE_ERROR: 'Error', STATUS_CODE_OK: 'OK' })[value] || 'Unset'
}

export function totals(values, count, suffix = '') {
  const entries = Object.entries(values || {})
  if (!entries.length) return '—'
  return entries.map(([key, result]) => {
    const coverage = result.knownCalls < count ? ` (${result.knownCalls}/${count} calls)` : ''
    return `${result.value} ${key}${suffix}${coverage}`
  }).join(' · ')
}

export function elapsed(start, end) {
  if (!start || !end || start === '0' || end === '0') return '—'
  const ns = BigInt(end) - BigInt(start)
  return ns < 0 ? '—' : `${(Number(ns) / 1e6).toLocaleString(undefined, { maximumFractionDigits: 2 })} ms`
}
