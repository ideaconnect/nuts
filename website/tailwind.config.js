/** @type {import('tailwindcss').Config} */
module.exports = {
  content: [
    "./_layouts/**/*.html",
    "./_includes/**/*.html",
    "./_posts/**/*.{html,md}",
    "./*.{html,md}",
    "./docs/**/*.{html,md}",
    "./contact/**/*.html", // permalinked pages in subdirs (e.g. contact/thanks.html) — not covered by ./*.{html,md}
    "./assets/js/**/*.js", // belt-and-suspenders; NOT a substitute for the hand-written classes in main.css
  ],
  // Only JS-toggled state hooks. The visual classes they pair with are hand-written
  // in assets/css/main.css, so they are already purge-immune; this is belt-and-suspenders.
  safelist: ["is-in", "is-scrolled"],
  theme: {
    extend: {
      colors: {
        nuts: {
          50: "#fdf8f0",
          100: "#f9eddb",
          200: "#f2d7b4",
          300: "#e9bb83",
          400: "#df9950",
          500: "#d88032",
          600: "#c96827",
          700: "#a75022",
          800: "#864122",
          900: "#6d371e",
        },
        base: "#0B0F1A",
        surface: "#121A2B",
        elevated: "#1B2740",
        "code-bg": "#0D1626",
        hairline: "#26314D",
        edge: "#39496F",
        ink: {
          50: "#EAEEF7",
          100: "#DCE3F0",
          200: "#C7D0E0",
          300: "#A5B0C4",
          400: "#8592AC",
          500: "#66748F",
          600: "#39496F",
          700: "#26314D",
          800: "#1B2740",
          900: "#121A2B",
          950: "#0B0F1A",
        },
        warm: { 400: "#E9A24C", 500: "#D88032", glow: "#FF9E52", hot: "#FFB877" },
        flux: { 300: "#7DD3FC", 400: "#38BDF8", 500: "#0EA5E9", 600: "#0284C7" },
        mid: "#8B5CF6",
        ok: "#34D399",
        warn: "#FBBF24",
        focus: "#FCD34D",
      },
      fontFamily: {
        display: ['"Space Grotesk"', "ui-sans-serif", "system-ui", "sans-serif"],
        sans: ["Inter", "ui-sans-serif", "system-ui", "-apple-system", "Segoe UI", "sans-serif"],
        mono: ['"JetBrains Mono"', "ui-monospace", "SFMono-Regular", "Menlo", "monospace"],
      },
      letterSpacing: { tightest: "-0.03em", tighter: "-0.02em", eyebrow: "0.16em" },
      maxWidth: { prose: "70ch" },
      borderRadius: { xl2: "1rem", xl3: "1.5rem" },
      backdropBlur: { glass: "14px", "glass-lg": "18px" },
      boxShadow: {
        card: "0 1px 1px rgba(0,0,0,.5),0 12px 30px -14px rgba(0,0,0,.7)",
        "ring-inner": "inset 0 1px 0 rgba(255,255,255,.06)",
        "glow-warm": "0 0 60px -12px rgba(255,158,82,.45)",
        "glow-cool": "0 0 60px -12px rgba(56,189,248,.5)",
        "btn-warm": "0 0 0 1px rgba(216,128,50,.35),0 8px 24px -10px rgba(255,158,82,.5)",
      },
      backgroundImage: {
        seam: "linear-gradient(90deg,#FF9E52 0%,#D88032 22%,#8B5CF6 50%,#38BDF8 78%,#22D3EE 100%)",
        "grid-faint":
          "linear-gradient(rgba(255,255,255,.03) 1px,transparent 1px),linear-gradient(90deg,rgba(255,255,255,.03) 1px,transparent 1px)",
      },
      transitionTimingFunction: {
        smooth: "cubic-bezier(.22,1,.36,1)",
        "out-expo": "cubic-bezier(.16,1,.3,1)",
        spring: "cubic-bezier(.34,1.56,.64,1)",
      },
    },
  },
  plugins: [require("@tailwindcss/typography")],
};
