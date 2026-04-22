import type { ButtonHTMLAttributes } from "react";

type Variant = "primary" | "secondary" | "ghost";

interface ButtonProps extends ButtonHTMLAttributes<HTMLButtonElement> {
  variant?: Variant;
}

const styles: Record<Variant, React.CSSProperties> = {
  primary: { background: "#4f46e5", color: "#fff" },
  secondary: { background: "#e5e7eb", color: "#111" },
  ghost: { background: "transparent", color: "#111" },
};

export function Button({ variant = "primary", style, ...rest }: ButtonProps) {
  return (
    <button
      {...rest}
      style={{
        padding: "6px 12px",
        borderRadius: 6,
        border: "1px solid #e5e7eb",
        cursor: "pointer",
        ...styles[variant],
        ...style,
      }}
    />
  );
}
