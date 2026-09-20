import { useEffect, useState } from 'react'
import { motionStateClass, useMountTransition } from '../hooks/useMountTransition'

export default function StatusToast({ message }) {
  const [lastMessage, setLastMessage] = useState(message)
  const { mounted, state } = useMountTransition(Boolean(message), '--toast-close')
  useEffect(() => {
    if (message) setLastMessage(message)
  }, [message])

  // Keep the live region present before text arrives. Retain the visual copy
  // during exit without announcing the same message a second time.
  return (
    <>
      <span className="sr-only" role="status">{message}</span>
      {mounted && (
        <div aria-hidden="true" className={`t-toast ${motionStateClass(state)} px-4 py-1.5 text-[11px] text-cyan border-b border-cyan/20 bg-cyan/5`}>
          {message || lastMessage}
        </div>
      )}
    </>
  )
}
