import React from 'react'

export default function AmbientBackground() {
  return (
    <div className="fixed inset-0 pointer-events-none overflow-hidden" aria-hidden="true">
      {/* Glow orbs */}
      <div
        className="absolute -top-[40%] -left-[20%] w-[900px] h-[900px] rounded-full opacity-[0.06]"
        style={{
          background: 'radial-gradient(circle, var(--color-accent) 0%, transparent 70%)',
          animation: 'drift 20s ease-in-out infinite',
        }}
      />
      <div
        className="absolute -bottom-[30%] -right-[15%] w-[700px] h-[700px] rounded-full opacity-[0.05]"
        style={{
          background: 'radial-gradient(circle, var(--color-cyan) 0%, transparent 70%)',
          animation: 'drift 25s ease-in-out infinite reverse',
        }}
      />
      <div
        className="absolute top-[30%] right-[10%] w-[500px] h-[500px] rounded-full opacity-[0.04]"
        style={{
          background: 'radial-gradient(circle, #9146FF 0%, transparent 70%)',
          animation: 'drift 18s ease-in-out infinite',
          animationDelay: '3s',
        }}
      />

      {/* Grid overlay */}
      <div
        className="absolute inset-0 opacity-[0.03]"
        style={{
          backgroundImage:
            'linear-gradient(rgba(255,255,255,0.08) 1px, transparent 1px), linear-gradient(90deg, rgba(255,255,255,0.08) 1px, transparent 1px)',
          backgroundSize: '80px 80px',
        }}
      />

      {/* Radial vignette */}
      <div
        className="absolute inset-0"
        style={{
          background: 'radial-gradient(ellipse at center, transparent 0%, var(--color-bg-primary) 70%)',
        }}
      />

      {/* Floating particles */}
      {[
        { color: 'var(--color-accent)', top: '15%', left: '10%', size: 4, dur: 7, delay: 0 },
        { color: 'var(--color-cyan)', top: '25%', left: '80%', size: 3, dur: 9, delay: 1.2 },
        { color: 'var(--color-success)', top: '60%', left: '20%', size: 5, dur: 11, delay: 2.4 },
        { color: '#9146FF', top: '70%', left: '70%', size: 4, dur: 8, delay: 3.6 },
        { color: 'var(--color-accent)', top: '40%', left: '50%', size: 3, dur: 13, delay: 4.8 },
        { color: 'var(--color-cyan)', top: '85%', left: '40%', size: 6, dur: 10, delay: 5.6 },
      ].map((p, i) => (
        <div
          key={i}
          className="absolute rounded-full opacity-30"
          style={{
            background: p.color,
            top: p.top,
            left: p.left,
            width: p.size,
            height: p.size,
            animation: `float ${p.dur}s ease-in-out infinite`,
            animationDelay: `${p.delay}s`,
          }}
        />
      ))}
    </div>
  )
}
