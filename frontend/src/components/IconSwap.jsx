export default function IconSwap({ active, from, to }) {
  return (
    <span className="t-icon-swap" data-state={active ? 'b' : 'a'} aria-hidden="true">
      <span className="t-icon" data-icon="a">{from}</span>
      <span className="t-icon" data-icon="b">{to}</span>
    </span>
  )
}
