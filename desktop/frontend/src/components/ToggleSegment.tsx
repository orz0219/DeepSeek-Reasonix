import { useT } from "../lib/i18n";

export function ToggleSegment({ value, disabled, onLabel, offLabel, onChange, }: {
    value: boolean;
    disabled: boolean;
    onLabel?: string;
    offLabel?: string;
    onChange: (value: boolean) => void;
}) {
    const t = useT();
    return (<div className="set-seg">
      <button type="button" className={`set-seg__btn${value ? " set-seg__btn--on" : ""}`} disabled={disabled} onClick={() => onChange(true)}>
        {onLabel ?? t("settings.toggleOn")}
      </button>
      <button type="button" className={`set-seg__btn${!value ? " set-seg__btn--on" : ""}`} disabled={disabled} onClick={() => onChange(false)}>
        {offLabel ?? t("settings.toggleOff")}
      </button>
    </div>);
}
