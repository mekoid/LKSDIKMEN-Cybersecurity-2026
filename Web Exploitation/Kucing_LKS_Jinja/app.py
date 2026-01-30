from flask import Flask, request, render_template, render_template_string

app = Flask(__name__, template_folder="templates")

@app.route("/", methods=["GET", "POST"])
def index():
    rendered = ""
    payload = ""
    if request.method == "POST":
        payload = request.form.get("msg", "")

        try:
            rendered = render_template_string(payload)
        except Exception as e:
            rendered = f"<pre>Error: {e}</pre>"

    return render_template("index.html", rendered=rendered, raw=payload)

if __name__ == "__main__":
    app.run(host="0.0.0.0", port=5000)
