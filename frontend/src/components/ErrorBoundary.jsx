import React from 'react'

export default class ErrorBoundary extends React.Component {
  constructor(props) {
    super(props)
    this.state = { hasError: false, error: null }
    this.handleRetry = this.handleRetry.bind(this)
  }

  static getDerivedStateFromError(error) {
    return { hasError: true, error }
  }

  componentDidCatch(error, info) {
    console.error('ErrorBoundary caught:', error, info.componentStack)
  }

  // Reset error state when children change (e.g., modal closed and reopened)
  componentDidUpdate(prevProps) {
    if (this.state.hasError && prevProps.resetKey !== this.props.resetKey) {
      this.setState({ hasError: false, error: null })
    }
  }

  handleRetry() {
    this.setState({ hasError: false, error: null })
  }

  render() {
    if (this.state.hasError) {
      // If a custom fallback was supplied, prefer it. Otherwise render an
      // inline error with a Retry button so the user isn't stuck.
      if (this.props.fallback) {
        return typeof this.props.fallback === 'function'
          ? this.props.fallback({ error: this.state.error, retry: this.handleRetry })
          : this.props.fallback
      }
      return (
        <div className="p-4 rounded-lg border border-danger/30 bg-danger/5 text-danger text-sm flex items-center justify-between gap-3">
          <div className="min-w-0">
            <div className="font-semibold mb-0.5">Something went wrong.</div>
            {this.state.error?.message ? (
              <div className="text-xs text-danger/70 truncate">{this.state.error.message}</div>
            ) : null}
          </div>
          <button
            onClick={this.handleRetry}
            className="shrink-0 px-3 py-1 rounded-md text-xs font-medium
              bg-danger/15 hover:bg-danger/25 transition-colors cursor-pointer"
          >
            Retry
          </button>
        </div>
      )
    }
    return this.props.children
  }
}
