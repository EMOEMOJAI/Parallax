/** Old agents send an empty done payload; explicit failures must stay failures. */
export function commandSucceeded(data, sawError = false) {
  if (sawError) return false
  try {
    const result = typeof data === 'string' ? JSON.parse(data) : data
    return result?.exit_ok !== false
  } catch {
    return true
  }
}
