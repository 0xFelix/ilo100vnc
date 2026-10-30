// ilo100vnc additions to noVNC, loaded by vnc.html as a module. noVNC shows
// its power panel when the server announces XVP. This adds "Power on",
// "Power off" and "BIOS Setup" (XVP codes 0x80-0x82, understood only by
// ilo100vnc), renames the standard buttons to what the iLO does, and asks
// for confirmation before every power action.
import UI from "./app/ui.js";

const panel = document.getElementById("noVNC_power");

function xvp(op) {
    if (UI.rfb && typeof UI.rfb._xvpOp === "function") {
        UI.rfb._xvpOp(1, op);
    } else {
        console.error("ilo100vnc: noVNC has no XVP support");
    }
}

// translate="no" keeps noVNC's localization from changing the labels.
function setLabel(b, label) {
    b.setAttribute("value", label);
    b.setAttribute("translate", "no");
}

function addButton(label, op) {
    const b = document.createElement("input");
    b.type = "button";
    setLabel(b, label);
    b.addEventListener("click", () => xvp(op));
    panel.appendChild(b);
}

function relabel(id, label) {
    const b = document.getElementById(id);
    if (b) {
        setLabel(b, label);
    }
}

if (panel) {
    relabel("noVNC_shutdown_button", "Shutdown (ACPI)");
    relabel("noVNC_reboot_button", "Power cycle");
    relabel("noVNC_reset_button", "Reset");
    addButton("Power on", 0x80);
    addButton("Power off", 0x82);
    addButton("BIOS Setup", 0x81);

    // Capture phase: runs before noVNC's own button handlers.
    panel.addEventListener("click", (e) => {
        if (e.target.type === "button" && !window.confirm(e.target.value + "?")) {
            e.stopPropagation();
        }
    }, true);
}
